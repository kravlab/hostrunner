// Package rules decides which host commands a container may run, from the
// allowlist in .devcontainer/hostrun.yaml.
//
// A rule is keyed by an argv prefix (`command: git push`); the rule with the
// longest matching prefix decides, and a command no rule matches is denied.
// A rule allows any arguments (`args: any`), none (`args: none`), or filters
// flags and positional arguments with allow or deny lists. Flag names are
// listed literally; a list for positional arguments or for the values of a
// flag holds globs (`allow`, `deny`) or regular expressions (`allow_regex`,
// `deny_regex`). Flags that take a value are declared under `flags.values`,
// which is how argv is split into flags, flag values and positional
// arguments without knowing the command's grammar.
//
// A regex is used as written: hostrunner adds no anchors, so it matches
// anywhere in a value unless the pattern says `^…$`. Like a glob it sees
// only the text of an argument: it does not resolve a path.
//
// A list for positional arguments or flag values may add a path check
// (`path: open` or `path: check`): every argument it applies to must name a
// workspace file, besides passing the patterns, which see the argument as
// given. This package cannot perform the check, which needs the workspace:
// Check reports the arguments concerned, and the caller checks them.
//
// Rules restrict argv only. A tool that reads configuration or hooks from
// the workspace can still be steered by whoever can write the workspace.
package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Denial explains why a command is not allowed. Rule is the matched rule's
// command, or empty when no rule applies.
type Denial struct {
	Rule   string
	Reason string
}

func (d *Denial) Error() string {
	if d.Rule == "" {
		return d.Reason
	}
	return fmt.Sprintf("denied by rule %q: %s", d.Rule, d.Reason)
}

// missingReason is the denial for every command when there is no config.
// It names no path: the client is in the container and must not learn host
// paths; the daemon logs where it looked.
const missingReason = "no rules file: every command is denied"

// Policy is a parsed allowlist. The zero value denies everything.
type Policy struct {
	rules   []*rule
	missing bool   // no config file: deny with missingReason
	digest  string // identifies the loaded file; see Digest
}

// missingDigest is the digest of a Policy loaded from a missing file.
const missingDigest = "missing"

// Digest identifies the file the Policy was loaded from (a SHA-256 of its
// content, or a fixed value when it was missing), so a running daemon can
// tell whether the rules file has changed since it started.
func (p *Policy) Digest() string { return p.digest }

// rule is one validated entry of the config.
type rule struct {
	command    []string
	name       string // command joined with single spaces, for messages
	args       string // "any", "none", or "" when flags/positional filter
	flags      *flagFilter
	positional *list // nil: positional arguments are unrestricted
}

// flagFilter restricts a rule's flags.
type flagFilter struct {
	names  *list            // nil: flags without a value are unrestricted
	values map[string]*list // flags that take a value; nil list: any value
}

// list is an allow or deny list of patterns: globs, or regexes when it was
// written as a regex list. The names of a flagFilter are a list too, whose
// patterns are flag names and are compared literally.
//
// A list for positional arguments or flag values may also carry a path
// check. Written without patterns, it is an empty deny list: the patterns
// let every value through and only the path check applies.
type list struct {
	allow    bool
	patterns []string         // globs or flag names
	regexps  []*regexp.Regexp // set instead of patterns in a regex list
	path     PathMode         // PathModeNone unless the list has a path check
}

// PathMode is how a path check hands a workspace file to the program.
type PathMode int

const (
	// PathModeNone: the argument is not checked as a path.
	PathModeNone PathMode = iota
	// PathModeOpen (`path: open`): the program gets the file the check
	// opened, so nothing swapped in after the check can redirect it.
	PathModeOpen
	// PathModeCheck (`path: check`): the program gets the argument as given
	// and opens it itself, after the check.
	PathModeCheck
)

// pathModes maps the values of a list's path key to their modes.
var pathModes = map[string]PathMode{"open": PathModeOpen, "check": PathModeCheck}

// matches reports whether any pattern matches s. A glob has to match all of
// s; a regex matches as regexp does, anywhere in s unless it anchors itself.
func (l *list) matches(s string) bool {
	return slices.ContainsFunc(l.patterns, func(p string) bool { return glob(p, s) }) ||
		slices.ContainsFunc(l.regexps, func(re *regexp.Regexp) bool { return re.MatchString(s) })
}

// Load reads the config at path. A missing file yields a Policy that denies
// every command; an unreadable or invalid one is an error naming path.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Policy{missing: true, digest: missingDigest}, nil
	}
	if err != nil {
		return nil, err
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	p.digest = hex.EncodeToString(sum[:])
	return p, nil
}

// Raw YAML shapes; Parse validates them into rules.
type (
	rawConfig struct {
		Rules []rawRule `yaml:"rules"`
	}
	rawRule struct {
		Command    string    `yaml:"command"`
		Args       string    `yaml:"args"`
		Flags      *rawFlags `yaml:"flags"`
		Positional *rawList  `yaml:"positional"`
	}
	rawList struct {
		Allow      []string `yaml:"allow"`
		Deny       []string `yaml:"deny"`
		AllowRegex []string `yaml:"allow_regex"`
		DenyRegex  []string `yaml:"deny_regex"`
		Path       string   `yaml:"path"`
	}
	rawFlags struct {
		Allow  []string           `yaml:"allow"`
		Deny   []string           `yaml:"deny"`
		Values map[string]rawList `yaml:"values"`
	}
)

// Parse validates a config. Unknown keys are errors, so a typo cannot
// silently widen or drop a restriction.
func Parse(data []byte) (*Policy, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("the rules file must hold a single YAML document")
	}
	p := &Policy{}
	seen := make(map[string]bool)
	for i, rr := range raw.Rules {
		r, err := newRule(rr)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i+1, err)
		}
		if seen[r.name] {
			return nil, fmt.Errorf("rule %d: duplicate command %q", i+1, r.name)
		}
		seen[r.name] = true
		p.rules = append(p.rules, r)
	}
	return p, nil
}

// newRule validates one raw rule.
func newRule(rr rawRule) (*rule, error) {
	r := &rule{command: strings.Fields(rr.Command)}
	r.name = strings.Join(r.command, " ")
	if len(r.command) == 0 {
		return nil, errors.New("command is empty")
	}
	if program := r.command[0]; strings.Contains(program, "/") && !filepath.IsAbs(program) {
		// A relative path resolves in the working directory, which the
		// container controls: it could plant any program there.
		return nil, fmt.Errorf("%q: the program must be a name or an absolute path", r.name)
	}
	switch rr.Args {
	case "any", "none":
		if rr.Flags != nil || rr.Positional != nil {
			return nil, fmt.Errorf("%q: args cannot be combined with flags or positional", r.name)
		}
		r.args = rr.Args
		return r, nil
	case "":
		if rr.Flags == nil && rr.Positional == nil {
			return nil, fmt.Errorf("%q: say what is allowed (args: any, args: none, flags or positional)", r.name)
		}
	default:
		return nil, fmt.Errorf("%q: args must be any or none, not %q", r.name, rr.Args)
	}
	var err error
	if rr.Positional != nil {
		if r.positional, err = newList(rawList(*rr.Positional)); err != nil {
			return nil, fmt.Errorf("%q positional: %w", r.name, err)
		}
		if r.positional == nil {
			// An empty section must not quietly mean "anything goes".
			return nil, fmt.Errorf("%q positional: set allow, deny, allow_regex, deny_regex or path", r.name)
		}
	}
	if rr.Flags != nil {
		if r.flags, err = newFlagFilter(*rr.Flags); err != nil {
			return nil, fmt.Errorf("%q flags: %w", r.name, err)
		}
	}
	return r, nil
}

// newFlagFilter validates a rule's flags section.
func newFlagFilter(rf rawFlags) (*flagFilter, error) {
	f := &flagFilter{values: make(map[string]*list)}
	var err error
	if f.names, err = newList(rawList{Allow: rf.Allow, Deny: rf.Deny}); err != nil {
		return nil, err
	}
	if f.names == nil && len(rf.Values) == 0 {
		// An empty section must not quietly mean "anything goes".
		return nil, errors.New("set allow, deny or values")
	}
	if f.names != nil {
		for _, name := range f.names.patterns {
			if err := checkFlagName(name); err != nil {
				return nil, err
			}
		}
	}
	for name, rl := range rf.Values {
		if err := checkFlagName(name); err != nil {
			return nil, err
		}
		if f.values[name], err = newList(rl); err != nil {
			return nil, fmt.Errorf("value of %s: %w", name, err)
		}
	}
	return f, nil
}

// newList validates a list, which is written with at most one of its four
// pattern keys and an optional path check; it returns nil when nothing is
// set. A null key counts as not set, an explicit empty one as set
// (`allow: []` allows nothing).
func newList(rl rawList) (*list, error) {
	var mode PathMode
	if rl.Path != "" {
		var ok bool
		if mode, ok = pathModes[rl.Path]; !ok {
			return nil, fmt.Errorf("path must be open or check, not %q", rl.Path)
		}
	}
	type key struct {
		name         string
		patterns     []string
		allow, regex bool
	}
	written := slices.DeleteFunc([]key{
		{name: "allow", patterns: rl.Allow, allow: true},
		{name: "deny", patterns: rl.Deny},
		{name: "allow_regex", patterns: rl.AllowRegex, allow: true, regex: true},
		{name: "deny_regex", patterns: rl.DenyRegex, regex: true},
	}, func(k key) bool { return k.patterns == nil })
	switch {
	case len(written) == 0 && mode == PathModeNone:
		return nil, nil
	case len(written) == 0:
		return &list{path: mode}, nil
	case len(written) > 1:
		names := make([]string, len(written))
		for i, k := range written {
			names[i] = k.name
		}
		return nil, fmt.Errorf("use only one of %s", strings.Join(names, ", "))
	}
	k := written[0]
	if !k.regex {
		return &list{allow: k.allow, patterns: k.patterns, path: mode}, nil
	}
	regexps, err := compileRegexps(k.patterns)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k.name, err)
	}
	return &list{allow: k.allow, regexps: regexps, path: mode}, nil
}

// compileRegexps compiles the patterns of a regex list as they are written.
// An error names the whole pattern, unescaped so that it reads as in the
// rules file, since regexp quotes only the part it could not parse.
func compileRegexps(patterns []string) ([]*regexp.Regexp, error) {
	regexps := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("`%s`: %w", pattern, err)
		}
		regexps = append(regexps, re)
	}
	return regexps, nil
}

// checkFlagName accepts -x and --name. A single-dash long name (-name) is
// refused because argv tokens like it are read as clusters of short flags,
// so such a rule could never match; "=" would never match either.
func checkFlagName(name string) error {
	short := strings.HasPrefix(name, "-") && !strings.HasPrefix(name, "--") && utf8.RuneCountInString(name) == 2
	long := strings.HasPrefix(name, "--") && len(name) > 2
	if !(short || long) || strings.Contains(name, "=") {
		return fmt.Errorf("%q is not a flag name (use -x or --name)", name)
	}
	return nil
}

// PathArg is an argument of an allowed command that a path check applies
// to. The path is argv[Index] without its first len(Prefix) bytes: Prefix
// is empty when the whole token is the path.
type PathArg struct {
	Index  int    // position of the token in argv
	Prefix string // what precedes the path in the token
	Mode   PathMode
}

// Check returns nil if argv is allowed, or a *Denial. For an allowed argv
// it also returns, in argv order, the arguments that a path check applies
// to; the caller has to perform those checks before running the command.
func (p *Policy) Check(argv []string) ([]PathArg, error) {
	if len(argv) == 0 {
		return nil, &Denial{Reason: "empty command"}
	}
	if p.missing {
		return nil, &Denial{Reason: missingReason}
	}
	var best *rule
	for _, r := range p.rules {
		if len(r.command) <= len(argv) && slices.Equal(r.command, argv[:len(r.command)]) &&
			(best == nil || len(r.command) > len(best.command)) {
			best = r
		}
	}
	if best == nil {
		return nil, &Denial{Reason: fmt.Sprintf("no rule allows %q", strings.Join(argv[:min(2, len(argv))], " "))}
	}
	var paths []PathArg
	if reason := best.check(argv[len(best.command):], &paths); reason != "" {
		return nil, &Denial{Rule: best.name, Reason: reason}
	}
	// check finds flag values as it goes but positional arguments at the end.
	slices.SortFunc(paths, func(a, b PathArg) int { return a.Index - b.Index })
	for i := range paths {
		paths[i].Index += len(best.command)
	}
	return paths, nil
}

// check applies the rule to the arguments after its command and returns
// why they are denied, or "" if they are allowed. It appends to paths the
// arguments a path check applies to, indexed into args.
func (r *rule) check(args []string, paths *[]PathArg) string {
	switch r.args {
	case "any":
		return ""
	case "none":
		if len(args) > 0 {
			return "arguments are not allowed"
		}
		return ""
	}
	var positional []int // indexes into args
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch {
		case tok == "--":
			for j := i + 1; j < len(args); j++ {
				positional = append(positional, j)
			}
			i = len(args)
		case tok == "-" || !strings.HasPrefix(tok, "-"):
			positional = append(positional, i)
		case strings.HasPrefix(tok, "--"):
			name, value, inline := strings.Cut(tok, "=")
			// A denied flag stays denied even if it also abbreviates a value
			// flag (--force vs --force-with-lease).
			if reason := r.flags.checkDenied(name); reason != "" {
				return reason
			}
			flag, reason := r.flags.resolveLong(name)
			if reason != "" {
				return reason
			}
			if flag != name && !inline {
				// Whether an abbreviation takes the next token is up to the
				// program; only the unambiguous form is filtered reliably.
				return fmt.Sprintf("abbreviated flag %s must be given its value as %s=value", name, name)
			}
			if _, takesValue := r.flags.valuesFor(flag); takesValue {
				prefix := name + "="
				if !inline {
					if i+1 == len(args) {
						return fmt.Sprintf("flag %s needs a value", flag)
					}
					i++
					value, prefix = args[i], ""
				}
				if reason := r.flags.checkValue(flag, value); reason != "" {
					return reason
				}
				r.flags.addPath(paths, flag, i, prefix)
				continue
			}
			if reason := r.flags.checkName(name, inline); reason != "" {
				return reason
			}
		default: // a cluster of short flags: -abc, -ovalue
			for j, c := range tok[1:] {
				flag := "-" + string(c)
				if _, takesValue := r.flags.valuesFor(flag); takesValue {
					prefix := tok[:1+j+utf8.RuneLen(c)]
					value := tok[len(prefix):]
					if value == "" {
						if i+1 == len(args) {
							return fmt.Sprintf("flag %s needs a value", flag)
						}
						i++
						value, prefix = args[i], ""
					}
					if reason := r.flags.checkValue(flag, value); reason != "" {
						return reason
					}
					r.flags.addPath(paths, flag, i, prefix)
					break
				}
				if reason := r.flags.checkName(flag, false); reason != "" {
					return reason
				}
			}
		}
	}
	if r.positional != nil {
		for _, i := range positional {
			arg := args[i]
			switch m := r.positional.matches(arg); {
			case r.positional.allow && !m:
				return fmt.Sprintf("argument %q is not allowed", arg)
			case !r.positional.allow && m:
				return fmt.Sprintf("argument %q is denied", arg)
			}
			if r.positional.path != PathModeNone {
				*paths = append(*paths, PathArg{Index: i, Mode: r.positional.path})
			}
		}
	}
	return ""
}

// strict reports whether flags are restricted by an allow list, in which
// case nothing is normalized: whatever is not listed is denied.
func (f *flagFilter) strict() bool {
	return f != nil && f.names != nil && f.names.allow
}

// valuesFor returns the value filter of a flag that takes a value.
func (f *flagFilter) valuesFor(flag string) (*list, bool) {
	if f == nil {
		return nil, false
	}
	l, ok := f.values[flag]
	return l, ok
}

// resolveLong maps a long flag to the declared value flag it abbreviates.
// Outside strict mode, programs such as git accept unambiguous prefixes
// (--proj for --project), so a prefix of exactly one value flag is treated
// as that flag and its value is still filtered; a prefix of several is
// denied as ambiguous.
func (f *flagFilter) resolveLong(name string) (string, string) {
	if f == nil || f.strict() {
		return name, ""
	}
	if _, ok := f.values[name]; ok {
		return name, ""
	}
	var candidates []string
	for flag := range f.values {
		if strings.HasPrefix(flag, "--") && strings.HasPrefix(flag, name) {
			candidates = append(candidates, flag)
		}
	}
	switch len(candidates) {
	case 0:
		return name, ""
	case 1:
		return candidates[0], ""
	}
	return "", fmt.Sprintf("flag %s is ambiguous", name)
}

// checkName checks a flag that takes no value. With an allow list the flag
// must be listed and must not carry an inline value; with a deny list see
// checkDenied.
func (f *flagFilter) checkName(flag string, inlineValue bool) string {
	if f == nil || f.names == nil {
		return ""
	}
	if !f.names.allow {
		return f.checkDenied(flag)
	}
	if !slices.Contains(f.names.patterns, flag) {
		return fmt.Sprintf("flag %s is not allowed", flag)
	}
	if inlineValue {
		return fmt.Sprintf("flag %s does not take a value", flag)
	}
	return ""
}

// checkDenied applies a deny list: a flag is denied if listed or, for a long
// flag, if it abbreviates a denied one (--forc for --force). Without a deny
// list it allows everything.
func (f *flagFilter) checkDenied(flag string) string {
	if f == nil || f.names == nil || f.names.allow {
		return ""
	}
	for _, denied := range f.names.patterns {
		if flag == denied || (strings.HasPrefix(flag, "--") && strings.HasPrefix(denied, flag)) {
			return fmt.Sprintf("flag %s is not allowed", flag)
		}
	}
	return ""
}

// checkValue filters the value of a flag that takes one.
func (f *flagFilter) checkValue(flag, value string) string {
	l, _ := f.valuesFor(flag)
	if l == nil {
		return ""
	}
	switch m := l.matches(value); {
	case l.allow && !m:
		return fmt.Sprintf("value %q of flag %s is not allowed", value, flag)
	case !l.allow && m:
		return fmt.Sprintf("value %q of flag %s is denied", value, flag)
	}
	return ""
}

// addPath appends to paths the value of flag at args[i], after prefix in its
// token, if the flag's list has a path check.
func (f *flagFilter) addPath(paths *[]PathArg, flag string, i int, prefix string) {
	if l, _ := f.valuesFor(flag); l != nil && l.path != PathModeNone {
		*paths = append(*paths, PathArg{Index: i, Prefix: prefix, Mode: l.path})
	}
}

// glob matches value against pattern, where * matches any sequence of
// characters including "/" (refs and paths must not slip past "+*"), ? one
// character, and everything else itself.
func glob(pattern, value string) bool {
	p, v := []rune(pattern), []rune(value)
	pi, vi := 0, 0
	star, mark := -1, 0 // last * in p, and where in v it started matching
	for vi < len(v) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == v[vi]) && p[pi] != '*':
			pi++
			vi++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, vi
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			vi = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
