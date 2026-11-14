package compose

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

type kind int

const (
	nullKind kind = iota
	scalarKind
	mapKind
	seqKind
)

// node is a YAML value with its aliases expanded and its merge keys applied.
type node struct {
	kind kind
	// tag is a scalar's resolved tag: !!str, !!int, !!float or !!bool.
	// Timestamps count as strings, as they do for Compose.
	tag string
	// text is a scalar as a string. A number or a boolean is formatted as
	// Compose formats it, from the value yaml.v3 decodes: 1.50 becomes 1.5.
	text string
	// keys are a mapping's keys: its own in file order, then the merged ones.
	keys  []string
	vals  map[string]*node
	items []*node
	line  int
}

// parse reads one YAML document whose top level is a mapping.
//
// yaml.v3 guards against alias expansion ("billion laughs") only while it
// decodes into Go values (allowedAliasRatio in its decode.go). Parsing into a
// yaml.Node expands nothing, so the builder below counts the values it adds
// through aliases and stops at maxAliasValues. It also refuses an alias inside
// the value it refers to, which yaml.v3's parser allows.
func parse(data []byte) (*node, error) {
	if len(data) > MaxFileBytes {
		return nil, fail("", "the file is larger than %d bytes", MaxFileBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fail("", "the file is empty")
		}
		return nil, fail("", "%v", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fail("", "%v", err)
		}
		return nil, fail("", "the file holds more than one YAML document")
	}
	if len(doc.Content) == 0 {
		return nil, fail("", "the file is empty")
	}
	b := &builder{active: map[*yaml.Node]bool{}, budget: maxAliasValues}
	root, err := b.build(doc.Content[0])
	if err != nil {
		return nil, err
	}
	switch root.kind {
	case nullKind:
		return nil, fail("", "the file is empty")
	case mapKind:
		return root, nil
	}
	return nil, fail("", "the top level of the file must be a mapping")
}

type builder struct {
	// active holds the collections being built, so that an alias to one of
	// them, from inside it, is caught instead of recursing forever.
	active map[*yaml.Node]bool
	// aliasDepth is above zero while an alias is being expanded.
	aliasDepth int
	// budget is how many more values alias expansion may add.
	budget int
}

func (b *builder) build(n *yaml.Node) (*node, error) {
	if b.aliasDepth > 0 {
		b.budget--
		if b.budget < 0 {
			return nil, fail("", "YAML aliases expand to more than %d values", maxAliasValues)
		}
	}
	switch n.Kind {
	case yaml.AliasNode:
		if n.Alias == nil {
			return nil, lineErr(n, "alias *%s has no anchor", n.Value)
		}
		if b.active[n.Alias] {
			return nil, lineErr(n, "alias *%s is inside the value it refers to", n.Value)
		}
		b.aliasDepth++
		v, err := b.build(n.Alias)
		b.aliasDepth--
		return v, err
	case yaml.ScalarNode:
		return scalar(n)
	case yaml.SequenceNode:
		if tag := n.ShortTag(); tag != "!!seq" {
			return nil, badTag(n, tag)
		}
		b.active[n] = true
		defer delete(b.active, n)
		out := &node{kind: seqKind, line: n.Line}
		for _, c := range n.Content {
			v, err := b.build(c)
			if err != nil {
				return nil, err
			}
			out.items = append(out.items, v)
		}
		return out, nil
	case yaml.MappingNode:
		return b.mapping(n)
	}
	return nil, lineErr(n, "unexpected YAML node")
}

func (b *builder) mapping(n *yaml.Node) (*node, error) {
	if tag := n.ShortTag(); tag != "!!map" {
		return nil, badTag(n, tag)
	}
	if len(n.Content)%2 != 0 {
		return nil, lineErr(n, "a mapping key has no value")
	}
	b.active[n] = true
	defer delete(b.active, n)
	out := &node{kind: mapKind, vals: map[string]*node{}, line: n.Line}
	seen := map[string]int{}
	var merge *yaml.Node
	for i := 0; i < len(n.Content); i += 2 {
		kn, vn := n.Content[i], n.Content[i+1]
		key, err := mapKey(kn)
		if err != nil {
			return nil, err
		}
		if line, dup := seen[key]; dup {
			return nil, lineErr(kn, "mapping key %q already defined at line %d", key, line)
		}
		seen[key] = kn.Line
		if isMerge(kn) {
			merge = vn
			continue
		}
		v, err := b.build(vn)
		if err != nil {
			return nil, err
		}
		out.keys = append(out.keys, key)
		out.vals[key] = v
	}
	if merge != nil {
		if err := b.merge(out, merge); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// merge applies a merge key: the keys of a mapping, or of each mapping in a
// list, that out does not have yet. A key in out wins over a merged one, and
// an earlier mapping in the list wins over a later one, as the YAML merge key
// type (https://yaml.org/type/merge.html) and yaml.v3 have it.
func (b *builder) merge(out *node, m *yaml.Node) error {
	sources := []*yaml.Node{m}
	if m.Kind == yaml.SequenceNode {
		sources = m.Content
	}
	for _, s := range sources {
		v, err := b.build(s)
		if err != nil {
			return err
		}
		if v.kind != mapKind {
			return lineErr(s, "a merge key (<<) takes a mapping or a list of mappings")
		}
		for _, k := range v.keys {
			if _, ok := out.vals[k]; !ok {
				out.keys = append(out.keys, k)
				out.vals[k] = v.vals[k]
			}
		}
	}
	return nil
}

func isMerge(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Value == "<<" && n.ShortTag() == "!!merge"
}

func mapKey(n *yaml.Node) (string, error) {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	if n.Kind != yaml.ScalarNode {
		return "", lineErr(n, "a mapping key must be a string")
	}
	return n.Value, nil
}

func scalar(n *yaml.Node) (*node, error) {
	tag := n.ShortTag()
	out := &node{kind: scalarKind, tag: "!!str", text: n.Value, line: n.Line}
	switch tag {
	case "!!null":
		return &node{kind: nullKind, line: n.Line}, nil
	case "!!str", "!!timestamp", "!!merge":
		return out, nil
	case "!!int", "!!float", "!!bool":
		var v any
		if err := n.Decode(&v); err != nil {
			// yaml.v3's message holds the value, which may be a secret.
			return nil, lineErr(n, "the value is not a valid %s", tag)
		}
		out.tag, out.text = tag, fmt.Sprint(v)
		return out, nil
	}
	return nil, badTag(n, tag)
}

func badTag(n *yaml.Node, tag string) error {
	if tag == "!reset" || tag == "!override" {
		return lineErr(n, "the %s tag is for merging Compose files, and hivemind reads one file", tag)
	}
	return lineErr(n, "the YAML tag %s is not supported", tag)
}

func lineErr(n *yaml.Node, format string, args ...any) error {
	return fail("", "line %d: %s", n.Line, fmt.Sprintf(format, args...))
}
