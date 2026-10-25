package rules

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Proposal is the narrowest edit of a rules file that lets an argv the
// rules refuse through: a new rule when none matched, or the matching
// rule's allow lists widened by what they refused.
type Proposal struct {
	Rule string // the command of the rule added or widened
	Data []byte // the whole file after the edit
	Diff string // the edit as a unified diff
}

// Propose proposes the narrowest edit of data, the content of a rules file
// (empty for a missing one), that allows argv; name names the file in the
// diff. It refuses argv the rules allow already, and a refusal it may not
// lift by widening lists: a guarded one (a deny list, an argv the rules
// cannot read, a rule with secrets). Only the lines of the rule added or
// widened change; the rest of the file is kept byte for byte, but for a
// final newline the file lacked, which it gains. A rule holding YAML
// anchors or aliases is not widened: its lists may be other rules' too.
// The result parses as Load parses a file and allows argv (see Allows), or
// Propose fails.
//
// A new rule's command is the program and the next token unless the rules
// read it as a flag. Its flags and arguments are read as the rules read
// argv without any flag declared to take a value: a flag given with a
// value in its token (--file=x) is declared to take one, allowing that
// value, in a new rule and in a widened one alike. Lists allow exactly what
// was given; an argument holding * or ? turns its list into anchored,
// quoted regular expressions, as a glob would match more.
func Propose(data []byte, name string, argv []string) (*Proposal, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command")
	}
	policy, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("the rules file does not parse: %w", err)
	}
	_, err = policy.Check(argv)
	d, approvable := ApprovableDenial(err)
	switch {
	case err == nil:
		return nil, errors.New("the rules allow it already")
	case !approvable:
		return nil, err
	case d.Guarded():
		return nil, errors.New("a guarded refusal gets no rule")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("the rules file does not parse: %w", err)
	}
	f := newRulesText(data)
	var rule *yaml.Node
	if d.Rule == "" {
		rule = proposedRule(argv)
		err = f.add(&doc, rule)
	} else {
		rule, err = widen(&doc, d, argv)
		if err == nil {
			err = f.replace(&doc, rule)
		}
	}
	if err != nil {
		return nil, err
	}
	out := f.bytes()
	if err := Allows(out, argv); err != nil {
		return nil, fmt.Errorf("the proposed rule does not allow it: %w", err)
	}
	return &Proposal{Rule: commandOf(rule), Data: out, Diff: f.diff(name)}, nil
}

// Allows checks data, the content of a rules file, as Load checks a file,
// and that its rules allow argv.
func Allows(data []byte, argv []string) error {
	policy, err := Parse(data)
	if err != nil {
		return err
	}
	_, err = policy.Check(argv)
	return err
}

// proposedRule returns the narrowest rule for argv, which no rule matches
// (see Propose).
func proposedRule(argv []string) *yaml.Node {
	command := argv[:1]
	if len(argv) > 1 && getoptToken(argv[1]) == tokenPositional {
		command = argv[:2]
	}
	rule := mapping()
	setValue(rule, "command", strNode(strings.Join(command, " ")))
	addArgs(rule, argv[len(command):])
	return rule
}

// addArgs gives rule lists allowing exactly args, read as the rules read
// argv with no flag declared to take a value, or `args: none` when there
// are none.
func addArgs(rule *yaml.Node, args []string) {
	if len(args) == 0 {
		setValue(rule, "args", strNode("none"))
		return
	}
	var flags, positional []string
	var values []string // flags given a value in their token, in order
	byFlag := map[string][]string{}
	ended := false
	for _, tok := range args {
		kind := getoptToken(tok)
		switch {
		case ended || kind == tokenPositional:
			positional = appendNew(positional, tok)
		case kind == tokenEnd:
			ended = true
		case kind == tokenLong:
			if name, value, inline := strings.Cut(tok, "="); inline {
				if _, ok := byFlag[name]; !ok {
					values = append(values, name)
				}
				byFlag[name] = appendNew(byFlag[name], value)
			} else {
				flags = appendNew(flags, tok)
			}
		default: // a cluster of short flags
			for _, c := range tok[1:] {
				flags = appendNew(flags, "-"+string(c))
			}
		}
	}
	fl := mapping()
	setValue(fl, "allow", sequence(flags))
	if len(values) > 0 {
		vs := mapping()
		for _, name := range values {
			setValue(vs, name, valueList(byFlag[name]))
		}
		setValue(fl, "values", vs)
	}
	setValue(rule, "flags", fl)
	pos := mapping()
	setList(pos, positional)
	setValue(rule, "positional", pos)
}

// valueList returns the list, a flow mapping, allowing exactly a flag's
// values.
func valueList(values []string) *yaml.Node {
	l := mapping()
	l.Style = yaml.FlowStyle
	setList(l, values)
	return l
}

// setList gives list, a mapping, the allow list of exactly items, or, when
// one holds a glob character, the allow_regex list matching exactly them.
func setList(list *yaml.Node, items []string) {
	if slices.ContainsFunc(items, hasGlobChar) {
		setValue(list, "allow_regex", sequence(exactRegexes(items)))
		return
	}
	setValue(list, "allow", sequence(items))
}

// widen widens the rule d refused argv by, in doc, to allow what its allow
// lists refused, and returns its node. An `args: none` rule gets lists
// allowing exactly the arguments given instead.
func widen(doc *yaml.Node, d *Denial, argv []string) (*yaml.Node, error) {
	rule := findRule(doc, d.Rule)
	if rule == nil {
		return nil, fmt.Errorf("rule %q is not in the rules file", d.Rule)
	}
	if sharesNodes(rule) {
		return nil, fmt.Errorf("rule %q uses YAML anchors or aliases: widen it by hand", d.Rule)
	}
	for _, r := range d.Reasons {
		switch r.Kind {
		case ReasonArgsNone:
			deleteKey(rule, "args")
			addArgs(rule, argv[len(strings.Fields(d.Rule)):])
		case ReasonNotAllowed:
			if err := allowRefused(rule, r.Refused); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("cannot widen the rule for: %s", r.Text)
		}
	}
	return rule, nil
}

// sharesNodes reports whether n, or a node in it, is anchored or an alias:
// an edit of it may reach beyond n.
func sharesNodes(n *yaml.Node) bool {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return true
	}
	return slices.ContainsFunc(n.Content, sharesNodes)
}

// allowRefused adds r to the allow list of rule that refused it.
func allowRefused(rule *yaml.Node, r Refused) error {
	flags := mapValue(rule, "flags")
	switch {
	case r.Kind == RefusedFlag && r.Inline:
		// The flag takes the value given in its token: declared so, it
		// allows that value.
		values := mapValue(flags, "values")
		if values == nil {
			values = mapping()
			setValue(flags, "values", values)
		}
		if list := valuesOf(values, r.Flag); list != nil {
			return allowIn(list, r.Text)
		}
		setValue(values, r.Flag, valueList([]string{r.Text}))
		return nil
	case r.Kind == RefusedFlag:
		return appendTo(mapValue(flags, "allow"), r.Flag)
	case r.Kind == RefusedValue:
		if list := valuesOf(mapValue(flags, "values"), r.Flag); list != nil {
			return allowIn(list, r.Text)
		}
		return fmt.Errorf("flag %s has no value list in the rules file", r.Flag)
	case r.Kind == RefusedArg:
		return allowIn(mapValue(rule, "positional"), r.Text)
	}
	return errors.New("the rules did not say what they refused")
}

// valuesOf returns the value list of flag in values, a flags.values
// mapping, or nil; flags are compared as a go-style rule compares them,
// which getopt names match too.
func valuesOf(values *yaml.Node, flag string) *yaml.Node {
	for i := 0; values != nil && i+1 < len(values.Content); i += 2 {
		if flagKey(values.Content[i].Value) == flagKey(flag) {
			return values.Content[i+1]
		}
	}
	return nil
}

// allowIn adds item to list, a list mapping with an allow or allow_regex
// list.
func allowIn(list *yaml.Node, item string) error {
	if seq := mapValue(list, "allow_regex"); seq != nil {
		return appendTo(seq, exactRegexes([]string{item})[0])
	}
	if hasGlobChar(item) {
		return fmt.Errorf("%q holds a glob character its allow list cannot match exactly", item)
	}
	return appendTo(mapValue(list, "allow"), item)
}

// appendTo appends item to seq, a sequence node, unless it holds it.
func appendTo(seq *yaml.Node, item string) error {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return errors.New("the allow list is not a list in the rules file")
	}
	if slices.ContainsFunc(seq.Content, func(n *yaml.Node) bool { return n.Value == item }) {
		return nil
	}
	seq.Content = append(seq.Content, strNode(item))
	return nil
}

// findRule returns the rule mapping in doc whose command is command, as
// the rules read it (its words joined by single spaces), or nil.
func findRule(doc *yaml.Node, command string) *yaml.Node {
	rules := rulesSequence(doc)
	if rules == nil {
		return nil
	}
	for _, r := range rules.Content {
		if commandOf(r) == command {
			return r
		}
	}
	return nil
}

// commandOf returns the command of rule as the rules read it.
func commandOf(rule *yaml.Node) string {
	return strings.Join(strings.Fields(scalar(mapValue(rule, "command"))), " ")
}

// rulesSequence returns the value of the rules key in doc, or nil.
func rulesSequence(doc *yaml.Node) *yaml.Node {
	if len(doc.Content) == 0 {
		return nil
	}
	return mapValue(doc.Content[0], "rules")
}

// hasGlobChar reports whether s holds a character an allow glob matches
// more than itself with.
func hasGlobChar(s string) bool { return strings.ContainsAny(s, "*?") }

// exactRegexes returns anchored regular expressions matching exactly items.
func exactRegexes(items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = "^" + regexp.QuoteMeta(s) + "$"
	}
	return out
}

// appendNew appends s to list unless it holds it already.
func appendNew(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// mapping returns an empty block mapping node.
func mapping() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode} }

// sequence returns a flow sequence of items.
func sequence(items []string) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle, Content: []*yaml.Node{}}
	for _, s := range items {
		seq.Content = append(seq.Content, strNode(s))
	}
	return seq
}

// strNode returns a string scalar node: tagged so, whatever s looks like.
func strNode(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

// scalar returns the value of n, a scalar node, or "".
func scalar(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// mapValue returns the value of key in m, a mapping node, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setValue sets key in m, a mapping node, to v, appending the key if m
// lacks it.
func setValue(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, strNode(key), v)
}

// deleteKey removes key from m, a mapping node.
func deleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = slices.Delete(m.Content, i, i+2)
			return
		}
	}
}

// rulesText is a rules file as lines, edited in one place: lines
// [from, to) of the original are replaced by added. eol ends every line
// added, as the file ends its lines.
type rulesText struct {
	lines    []string // the original, each with its line ending
	eol      string
	from, to int
	added    []string
}

// newRulesText splits data into lines, giving the last one a line ending.
func newRulesText(data []byte) *rulesText {
	s := string(data)
	eol := "\n"
	if strings.Contains(s, "\r\n") {
		eol = "\r\n"
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += eol
	}
	lines := strings.SplitAfter(s, "\n")
	return &rulesText{lines: lines[:len(lines)-1], eol: eol}
}

// add appends rule to the rules of doc: after the last rule, after an
// empty rules key, or under a new rules key at the end of the file.
func (f *rulesText) add(doc *yaml.Node, rule *yaml.Node) error {
	rules := rulesSequence(doc)
	switch {
	case rules == nil:
		f.from, f.to = len(f.lines), len(f.lines)
		item, err := f.encodeItem(rule, "  ")
		f.added = append([]string{"rules:" + f.eol}, item...)
		return err
	case rules.Kind == yaml.ScalarNode && rules.Tag == "!!null":
		f.from, f.to = rules.Line, rules.Line // after the line of the key
	case rules.Kind != yaml.SequenceNode || rules.Style&yaml.FlowStyle != 0 || len(rules.Content) == 0:
		return errors.New("the rules are not a block list: add the rule by hand")
	default:
		_, end := f.span(rules, len(rules.Content)-1)
		f.from, f.to = end, end
	}
	indent := "  "
	if rules.Kind == yaml.SequenceNode {
		indent = strings.Repeat(" ", max(rules.Content[0].Column-3, 0))
	}
	item, err := f.encodeItem(rule, indent)
	f.added = item
	return err
}

// replace replaces the lines of rule, one of the rules of doc, with its
// edited form.
func (f *rulesText) replace(doc *yaml.Node, rule *yaml.Node) error {
	rules := rulesSequence(doc)
	f.from, f.to = f.span(rules, slices.Index(rules.Content, rule))
	// The comments before and after the rule stay where they are, outside
	// its lines, so the edited form must not repeat them.
	rule.HeadComment = ""
	if len(rule.Content) > 0 {
		rule.Content[0].HeadComment = ""
	}
	clearTrailingComments(rule)
	item, err := f.encodeItem(rule, strings.Repeat(" ", max(rule.Column-3, 0)))
	f.added = item
	return err
}

// clearTrailingComments drops the foot comments of n and of the last nodes
// within it, which yaml.v3 attaches the comments after a rule to.
func clearTrailingComments(n *yaml.Node) {
	n.FootComment = ""
	switch {
	case n.Kind == yaml.MappingNode && len(n.Content) >= 2:
		n.Content[len(n.Content)-2].FootComment = ""
		clearTrailingComments(n.Content[len(n.Content)-1])
	case n.Kind == yaml.SequenceNode && len(n.Content) > 0:
		clearTrailingComments(n.Content[len(n.Content)-1])
	}
}

// span returns the lines [from, to) of the i-th rule of rules: from its
// first line to its last one that is neither blank, a comment nor a
// document marker, which belong to what follows.
func (f *rulesText) span(rules *yaml.Node, i int) (int, int) {
	from := rules.Content[i].Line - 1
	to := len(f.lines)
	if i+1 < len(rules.Content) {
		to = rules.Content[i+1].Line - 1
	}
	for to > from+1 {
		t := strings.TrimSpace(f.lines[to-1])
		if t != "" && !strings.HasPrefix(t, "#") && t != "..." && t != "---" {
			break
		}
		to--
	}
	return from, to
}

// encodeItem renders rule as an item of a block list, indented by indent,
// its lines ending as the file's do.
func (f *rulesText) encodeItem(rule *yaml.Node, indent string) ([]string, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{rule}}); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	lines := strings.SplitAfter(b.String(), "\n")
	lines = lines[:len(lines)-1]
	for i := range lines {
		lines[i] = indent + strings.TrimSuffix(lines[i], "\n") + f.eol
	}
	return lines, nil
}

// bytes returns the edited file.
func (f *rulesText) bytes() []byte {
	var b strings.Builder
	for _, l := range f.lines[:f.from] {
		b.WriteString(l)
	}
	for _, l := range f.added {
		b.WriteString(l)
	}
	for _, l := range f.lines[f.to:] {
		b.WriteString(l)
	}
	return []byte(b.String())
}

// diff returns the edit as a unified diff of name, with three lines of
// context.
func (f *rulesText) diff(name string) string {
	const context = 3
	cs, ce := max(f.from-context, 0), min(f.to+context, len(f.lines))
	oldLen := ce - cs
	newLen := oldLen - (f.to - f.from) + len(f.added)
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n@@ -%s +%s @@\n", name, name, hunkRange(cs, oldLen), hunkRange(cs, newLen))
	line := func(prefix, l string) { b.WriteString(prefix + strings.TrimRight(l, "\r\n") + "\n") }
	for _, l := range f.lines[cs:f.from] {
		line(" ", l)
	}
	for _, l := range f.lines[f.from:f.to] {
		line("-", l)
	}
	for _, l := range f.added {
		line("+", l)
	}
	for _, l := range f.lines[f.to:ce] {
		line(" ", l)
	}
	return b.String()
}

// hunkRange renders a hunk's range of n lines from line start (0-based).
func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}
