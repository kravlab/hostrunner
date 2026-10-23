package rules

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// flagStyle is the grammar a rule reads the program's argv with, set by the
// rule's `flag_style` key.
type flagStyle int

const (
	// styleGetopt, the default: -abc is three short flags, a long flag has
	// two dashes and may be abbreviated.
	styleGetopt flagStyle = iota
	// styleGo: one or two dashes name the same flag, as Go's flag package and
	// urfave/cli v1 to v3 read it. See checkGo.
	styleGo
)

// flagStyles maps the values of a rule's flag_style key to their styles.
var flagStyles = map[string]flagStyle{"getopt": styleGetopt, "go": styleGo}

// flagKey is the name a go-style flag is stored and looked up under: its
// name without the leading dashes, so -name and --name are one flag.
func flagKey(name string) string { return strings.TrimLeft(name, "-") }

// checkGo applies a go-style rule to the arguments after its command, as
// check does under getopt, adding every reason to found.
//
// The parsers this style stands for differ. Go's flag package and urfave/cli
// v1 and v2 stop reading flags at the first positional argument (v1 leaf
// commands move known flags in front first); urfave/cli v3 reads flags after
// positional arguments, stops at a single-dash token not followed by a
// letter (-1), trims whitespace around a token, and 3.10.1 drops every token
// after a lone "-". Up to the first token on which they may disagree, every
// token is read the same way by all of them. From that token on, each token
// has to pass every reading: as a positional argument, and as a flag or a
// flag's value. A path check cannot apply to a token read in more than one
// way: the daemon would rewrite a token the program may read as something
// else. Such a token can get a reason for each reading it fails (as a
// positional argument, and as a flag or its value), each true for some
// parser.
func (r *rule) checkGo(args []string, paths *[]PathArg, found *findings) {
	i := 0
agreed:
	for ; i < len(args); i++ {
		tok := args[i]
		switch {
		case tok == "--":
			r.goPositionals(args, i+1, paths, found)
			return
		case isGoFlag(tok):
			if r.goFlag(args, i, false, paths, found) {
				i++
			}
		case tok == "-" || !strings.HasPrefix(tok, "-") && strings.TrimSpace(tok) == tok:
			// Every parser reads the first positional argument as one; they
			// disagree on what follows it.
			r.goPositional(args, i, false, paths, found)
			i++
			break agreed
		default: // -1, or whitespace around the token: read differently already
			break agreed
		}
	}
	value := false // args[i] is a flag's value in the flag reading
	for ; i < len(args); i++ {
		t := strings.TrimSpace(args[i])
		if !value && t == "--" {
			// urfave/cli v3 ends the flags here; the other parsers read it
			// and everything after it as positional arguments already.
			r.goPositional(args, i, true, paths, found)
			r.goPositionals(args, i+1, paths, found)
			return
		}
		flag := !value && strings.HasPrefix(t, "-") && t != "-"
		r.goPositional(args, i, value || flag, paths, found)
		if value {
			value = false // goFlag has checked it as the value
			continue
		}
		if flag {
			value = r.goFlag(args, i, true, paths, found)
		}
	}
}

// isGoFlag reports whether every Go parser reads tok as a flag: two dashes,
// or one dash and an ASCII letter (urfave/cli v3 reads -1 as a positional
// argument), and no whitespace around it (v3 trims it, the others do not).
func isGoFlag(tok string) bool {
	if strings.TrimSpace(tok) != tok {
		return false
	}
	if strings.HasPrefix(tok, "--") {
		return true
	}
	return len(tok) > 1 && tok[0] == '-' && ('a' <= tok[1] && tok[1] <= 'z' || 'A' <= tok[1] && tok[1] <= 'Z')
}

// goPositionals checks args[from:] as positional arguments, which every
// parser reads them as.
func (r *rule) goPositionals(args []string, from int, paths *[]PathArg, found *findings) {
	for i := from; i < len(args); i++ {
		r.goPositional(args, i, false, paths, found)
	}
}

// goPositional checks args[i] as a positional argument, adding to found
// why it is refused. ambiguous is set when a parser may read it as a flag
// or a flag's value instead. A token with whitespace around it is checked
// as given and trimmed, as urfave/cli v3 reads it; like an ambiguous one,
// it cannot take a path check.
func (r *rule) goPositional(args []string, i int, ambiguous bool, paths *[]PathArg, found *findings) {
	tok := args[i]
	readings := textReadings(tok)
	for _, arg := range readings {
		if !r.checkPositional(arg, found) {
			return
		}
	}
	if r.positional == nil || r.positional.path == PathModeNone {
		return
	}
	if ambiguous || len(readings) > 1 {
		found.add(ReasonUnreadable, fmt.Sprintf("argument %q is read differently by Go flag parsers: a path check cannot apply to it", tok))
		return
	}
	*paths = append(*paths, PathArg{Index: i, Mode: r.positional.path})
}

// goFlag checks args[i] as a flag the way the Go parsers read it: one or two
// dashes, an exact name, a value inline or in the next token, and, for a
// single-dash name the rule does not know, possibly a cluster of short flags.
// It adds to found why the flag is refused and reports whether the flag
// takes the next token as its value. ambiguous is set when a parser may read
// the token, or the token after it, as something else (see checkGo).
func (r *rule) goFlag(args []string, i int, ambiguous bool, paths *[]PathArg, found *findings) bool {
	tok := args[i]
	t := strings.TrimSpace(tok)
	name, _, inline := strings.Cut(t, "=")
	shown := name // the flag as written in argv, for messages
	if t != tok {
		given, _, _ := strings.Cut(tok, "=")
		shown = strconv.Quote(given) // whitespace and all
	}
	if strings.HasPrefix(t, "---") {
		found.add(ReasonUnreadable, fmt.Sprintf("flag %s has more than two dashes", shown))
		return false
	}
	f := r.flags
	if f == nil {
		return false
	}
	key := flagKey(name)
	denied := func(key string) bool {
		return f.names != nil && !f.names.allow && slices.Contains(f.names.patterns, key)
	}
	if denied(key) {
		found.add(ReasonDenied, fmt.Sprintf("flag %s is not allowed", shown))
		// A denied value flag still takes the next token as its value, as
		// under getopt, so that token gets no reason of its own.
		_, takesValue := f.values[key]
		return takesValue && !inline && i+1 < len(args)
	}
	if l, ok := f.values[key]; ok {
		return goValue(args, i, l, shown, ambiguous, paths, found)
	}
	if f.names != nil && f.names.allow && !slices.Contains(f.names.patterns, key) {
		found.add(ReasonNotAllowed, fmt.Sprintf("flag %s is not allowed", shown))
		return false
	}
	if inline {
		// A boolean flag takes it: --dry-run=false turns the flag off.
		found.add(ReasonUnreadable, fmt.Sprintf("flag %s does not take a value", shown))
		return false
	}
	allowed := f.names != nil && f.names.allow // and listed, or denied above
	if !allowed && !strings.HasPrefix(name, "--") && utf8.RuneCountInString(key) > 1 {
		// With UseShortOptionHandling, a name the program does not define is
		// a cluster of short flags. Where a value flag sits in a cluster,
		// urfave/cli versions read its value differently. A name an allow
		// list gives is one the program defines, and every parser tries the
		// whole name first.
		for _, c := range key {
			letter := string(c)
			if denied(letter) {
				found.add(ReasonDenied, fmt.Sprintf("flag -%s in %s is not allowed", letter, shown))
				return false
			}
			if _, ok := f.values[letter]; ok {
				found.add(ReasonUnreadable, fmt.Sprintf("flag -%s in %s takes a value", letter, shown))
				return false
			}
		}
	}
	return false
}

// goValue checks the value of the value flag at args[i], named name in
// messages and filtered by l, adds to found why it is refused, and appends
// it to paths if l has a path check. It reports whether the value is the
// next token.
func goValue(args []string, i int, l *list, name string, ambiguous bool, paths *[]PathArg, found *findings) bool {
	tok := args[i]
	t := strings.TrimSpace(tok)
	var values []string // the value's readings
	at, prefix := i, tok[:strings.Index(tok, "=")+1]
	if _, value, inline := strings.Cut(t, "="); inline {
		// Some urfave/cli versions cut the value from the token as given,
		// others from the trimmed token.
		values = []string{value}
		if _, given, _ := strings.Cut(tok, "="); given != value {
			values = append(values, given)
		}
	} else {
		if i+1 == len(args) {
			found.add(ReasonUnreadable, fmt.Sprintf("flag %s needs a value", name))
			return false
		}
		at, prefix = i+1, ""
		next := args[at]
		if ambiguous && strings.HasPrefix(strings.TrimSpace(next), "-") {
			// urfave/cli v1 leaf commands move a known flag in front together
			// with the next token only if that token is not a flag.
			found.add(ReasonUnreadable, fmt.Sprintf("flag %s is followed by %q: Go flag parsers disagree on its value here", name, next))
			return false
		}
		values = textReadings(next)
	}
	for _, v := range values {
		if !checkFlagValue(l, name, v, found) {
			return at != i
		}
	}
	if l != nil && l.path != PathModeNone {
		if ambiguous || t != tok || len(values) > 1 {
			found.add(ReasonUnreadable, fmt.Sprintf("value %q of flag %s is read differently by Go flag parsers: a path check cannot apply to it", values[0], name))
			return at != i
		}
		*paths = append(*paths, PathArg{Index: at, Prefix: prefix, Mode: l.path})
	}
	return at != i
}

// textReadings returns the texts a parser may see for a token: as given
// and, with whitespace around it, trimmed as urfave/cli v3 reads it.
func textReadings(tok string) []string {
	if t := strings.TrimSpace(tok); t != tok {
		return []string{tok, t}
	}
	return []string{tok}
}
