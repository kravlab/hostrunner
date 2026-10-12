package rules

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A rule's injection is what it adds to its command after the checks
// (`inject:`); its secrets are the values it obtains on the host for that
// (`secrets:`), which the container never sees. The rule alone places
// them: injected tokens are not checked by the rule's lists, which see the
// container's argv only, and the container cannot move them. See ADR 0006.

// SourceKind says where a secret's value comes from.
type SourceKind int

const (
	// SourceEnv: a variable of the daemon's environment.
	SourceEnv SourceKind = iota + 1
	// SourceCommand: the stdout of a host command, run in the command's
	// working directory.
	SourceCommand
	// SourceFile: the content of a host file, absolute or relative to the
	// command's working directory.
	SourceFile
)

// defaultTimeout is how long a command source may run unless its secret
// says otherwise.
const defaultTimeout = 30 * time.Second

// Secret is a named secret of a rule and its source. The caller obtains
// its value for every request; this package never does.
type Secret struct {
	Name    string
	Kind    SourceKind
	Env     string        // SourceEnv: the variable's name
	Command []string      // SourceCommand: the argv to run
	Timeout time.Duration // SourceCommand: how long it may run
	File    string        // SourceFile: the path as written
}

// Injection is what a rule adds to its command. Each field holds
// templates the caller renders with the request's secret values.
type Injection struct {
	// ProgramArgs go right after the program, before the rest of the
	// rule's command (global flags such as git -c).
	ProgramArgs []Template
	// CommandArgs go right after the rule's command, before the
	// container's arguments.
	CommandArgs []Template
	// TrailingArgs go after the container's arguments. They are positional
	// only, so a -- from the container cannot turn one into an argument
	// the program echoes.
	TrailingArgs []Template
}

// empty reports whether the injection adds nothing.
func (in Injection) empty() bool {
	return len(in.ProgramArgs) == 0 && len(in.CommandArgs) == 0 && len(in.TrailingArgs) == 0
}

// Template is a string of an injection: literal text with references to
// secrets (`${name}`); `$$` is a literal `$`.
type Template struct {
	parts []templatePart
}

// templatePart is literal text, or a reference to a secret when secret is
// set.
type templatePart struct {
	text   string
	secret string
}

// Render returns t with each reference replaced by the value values holds
// for it. Parse made sure every reference names a secret of the rule.
func (t Template) Render(values map[string]string) string {
	var b strings.Builder
	for _, p := range t.parts {
		if p.secret != "" {
			b.WriteString(values[p.secret])
		} else {
			b.WriteString(p.text)
		}
	}
	return b.String()
}

// secretName is the grammar of a secret's name.
var secretName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// parseTemplate parses s, a template as the rules file writes it.
func parseTemplate(s string) (Template, error) {
	var t Template
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			t.parts = append(t.parts, templatePart{text: text.String()})
			text.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			text.WriteByte(s[i])
			continue
		}
		switch {
		case strings.HasPrefix(s[i:], "$$"):
			text.WriteByte('$')
			i++
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return Template{}, fmt.Errorf("%q: unterminated ${", s)
			}
			name := s[i+2 : i+end]
			if !secretName.MatchString(name) {
				return Template{}, fmt.Errorf("%q: ${%s} is not a secret name", s, name)
			}
			flush()
			t.parts = append(t.parts, templatePart{secret: name})
			i += end
		default:
			// A lone $ is more likely a typo than literal text.
			return Template{}, fmt.Errorf("%q: a lone $ (write $$ for a literal $)", s)
		}
	}
	flush()
	return t, nil
}

// references returns the secrets t refers to.
func (t Template) references() []string {
	var names []string
	for _, p := range t.parts {
		if p.secret != "" {
			names = append(names, p.secret)
		}
	}
	return names
}

// Raw YAML shapes of secrets and injections; newSecrets and newInjection
// validate them.
type (
	rawSecret struct {
		Env     string   `yaml:"env"`
		Command []string `yaml:"command"`
		Timeout string   `yaml:"timeout"`
		File    string   `yaml:"file"`
	}
	rawInject struct {
		ProgramArgs  []string `yaml:"program_args"`
		CommandArgs  []string `yaml:"command_args"`
		TrailingArgs []string `yaml:"trailing_args"`
	}
)

// newSecrets validates a rule's secrets, sorted by name so that errors and
// the order the caller obtains them in do not depend on map order.
func newSecrets(raw map[string]rawSecret) ([]Secret, error) {
	var secrets []Secret
	for _, name := range slices.Sorted(maps.Keys(raw)) {
		if !secretName.MatchString(name) {
			return nil, fmt.Errorf("secret %q: a name is lower-case letters, digits and _, starting with a letter", name)
		}
		sec, err := newSecret(name, raw[name])
		if err != nil {
			return nil, fmt.Errorf("secret %q: %w", name, err)
		}
		secrets = append(secrets, sec)
	}
	return secrets, nil
}

// newSecret validates one secret: exactly one source, and a timeout only
// for a command source.
func newSecret(name string, rs rawSecret) (Secret, error) {
	sec := Secret{Name: name}
	var sources []string
	if rs.Env != "" {
		sources = append(sources, "env")
		sec.Kind, sec.Env = SourceEnv, rs.Env
	}
	if rs.Command != nil {
		sources = append(sources, "command")
		sec.Kind, sec.Command = SourceCommand, rs.Command
		if len(rs.Command) == 0 || rs.Command[0] == "" {
			return Secret{}, errors.New("command must name a program")
		}
	}
	if rs.File != "" {
		sources = append(sources, "file")
		sec.Kind, sec.File = SourceFile, rs.File
	}
	switch len(sources) {
	case 0:
		return Secret{}, errors.New("set its source (command, env or file)")
	case 1:
	default:
		return Secret{}, fmt.Errorf("use only one of %s", strings.Join(sources, ", "))
	}
	if rs.Timeout != "" && sec.Kind != SourceCommand {
		return Secret{}, errors.New("timeout applies to a command source only")
	}
	if sec.Kind == SourceCommand {
		sec.Timeout = defaultTimeout
		if rs.Timeout != "" {
			d, err := time.ParseDuration(rs.Timeout)
			if err != nil || d <= 0 {
				return Secret{}, fmt.Errorf("timeout must be a positive duration such as 30s, not %q", rs.Timeout)
			}
			sec.Timeout = d
		}
	}
	return sec, nil
}

// newInjection validates a rule's injection against its secrets: every
// reference must name one of them, and every one of them must be used, as
// an unused secret is most likely a typo and its source would be obtained
// for nothing.
func newInjection(raw *rawInject, secrets []Secret) (Injection, error) {
	var in Injection
	used := make(map[string]bool)
	if raw != nil {
		for _, field := range []struct {
			key  string
			raw  []string
			into *[]Template
		}{
			{"program_args", raw.ProgramArgs, &in.ProgramArgs},
			{"command_args", raw.CommandArgs, &in.CommandArgs},
			{"trailing_args", raw.TrailingArgs, &in.TrailingArgs},
		} {
			for _, s := range field.raw {
				if field.key == "trailing_args" && strings.HasPrefix(s, "-") {
					return Injection{}, fmt.Errorf("inject.trailing_args: %q starts with -: trailing arguments are positional, add flags to program_args or command_args", s)
				}
				t, err := newTemplate(s, secrets, used)
				if err != nil {
					return Injection{}, fmt.Errorf("inject.%s: %w", field.key, err)
				}
				*field.into = append(*field.into, t)
			}
		}
	}
	for _, sec := range secrets {
		if !used[sec.Name] {
			return Injection{}, fmt.Errorf("secret %q is not used by inject", sec.Name)
		}
	}
	return in, nil
}

// newTemplate parses s and checks that it refers to secrets only, marking
// those in used.
func newTemplate(s string, secrets []Secret, used map[string]bool) (Template, error) {
	t, err := parseTemplate(s)
	if err != nil {
		return Template{}, err
	}
	for _, name := range t.references() {
		if !slices.ContainsFunc(secrets, func(sec Secret) bool { return sec.Name == name }) {
			return Template{}, fmt.Errorf("%q refers to unknown secret %q", s, name)
		}
		used[name] = true
	}
	return t, nil
}

// checkStrictLists refuses a rule with secrets whose lists leave part of
// argv unrestricted: a program has more ways to give its secret away
// (verbose output, a header dump, a proxy) than a deny list can name, and
// a missing list is no restriction at all. Only allow lists are accepted,
// or args: any with secrets_with_any_args as a deliberate choice.
func (r *rule) checkStrictLists(withAnyArgs bool) error {
	if len(r.secrets) == 0 || r.args == "none" {
		if withAnyArgs {
			return fmt.Errorf("%q: secrets_with_any_args needs secrets and args: any", r.name)
		}
		return nil
	}
	if r.args == "any" {
		if !withAnyArgs {
			return fmt.Errorf("%q: a rule with secrets and args: any needs secrets_with_any_args: true", r.name)
		}
		return nil
	}
	if withAnyArgs {
		return fmt.Errorf("%q: secrets_with_any_args needs args: any", r.name)
	}
	if r.flags == nil || r.flags.names == nil || !r.flags.names.allow {
		return fmt.Errorf("%q: a rule with secrets needs flags.allow (use [] for no flags)", r.name)
	}
	for _, name := range slices.Sorted(maps.Keys(r.flags.values)) {
		if l := r.flags.values[name]; l == nil || !l.allow {
			return fmt.Errorf("%q: a rule with secrets needs an allow or allow_regex list for the value of %s", r.name, name)
		}
	}
	if r.positional == nil || !r.positional.allow {
		return fmt.Errorf("%q: a rule with secrets needs positional with allow or allow_regex", r.name)
	}
	return nil
}

// ErrTrailingFlag is Assemble's error for a rendered trailing argument
// that would read as a flag. Its text holds no value.
var ErrTrailingFlag = errors.New("an argument the rule adds after the container's starts with -")

// Assemble returns the argv to run for argv, the container's argv after
// the caller's own replacements (path: open), with the allowing rule's
// injection rendered from values, the secret values by name: program,
// ProgramArgs, rest of the rule's command, CommandArgs, the container's
// arguments, TrailingArgs. A rendered trailing argument starting with -
// is ErrTrailingFlag.
func (a Allowed) Assemble(argv []string, values map[string]string) ([]string, error) {
	if a.Inject.empty() {
		return argv, nil
	}
	render := func(ts []Template) []string {
		out := make([]string, len(ts))
		for i, t := range ts {
			out[i] = t.Render(values)
		}
		return out
	}
	trailing := render(a.Inject.TrailingArgs)
	if slices.ContainsFunc(trailing, func(arg string) bool { return strings.HasPrefix(arg, "-") }) {
		return nil, ErrTrailingFlag
	}
	out := append([]string{argv[0]}, render(a.Inject.ProgramArgs)...)
	out = append(out, argv[1:a.commandLen]...)
	out = append(out, render(a.Inject.CommandArgs)...)
	out = append(out, argv[a.commandLen:]...)
	return append(out, trailing...), nil
}
