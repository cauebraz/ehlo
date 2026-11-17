package monitoring

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// kv is one key of an ordered mapping.
type kv struct {
	Key   string
	Value any
}

// ordered is a mapping that keeps its key order in YAML.
type ordered []kv

// marshalYAML encodes documents as YAML with two-space indentation, one
// document after another separated by ---. A multi-line string becomes a
// literal block.
func marshalYAML(docs ...any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, d := range docs {
		n, err := node(d)
		if err != nil {
			return nil, err
		}
		if err := enc.Encode(n); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func node(v any) (*yaml.Node, error) {
	switch v := v.(type) {
	case ordered:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, e := range v {
			val, err := node(e.Value)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, str(e.Key), val)
		}
		return n, nil
	case map[string]string:
		o := ordered{}
		for _, k := range slices.Sorted(maps.Keys(v)) {
			o = append(o, kv{k, v[k]})
		}
		return node(o)
	case map[string]any:
		o := ordered{}
		for _, k := range slices.Sorted(maps.Keys(v)) {
			o = append(o, kv{k, v[k]})
		}
		return node(o)
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range v {
			c, err := node(e)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, c)
		}
		return n, nil
	case string:
		return str(v), nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(string(v), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: string(v)}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return nil, fmt.Errorf("yaml: unsupported value %T", v)
}

// str is a string scalar; a multi-line one is a literal block. Words YAML
// 1.1 reads as booleans are double quoted, because Kubernetes and Helm parse
// YAML 1.1.
func str(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	switch {
	case strings.Contains(s, "\n"):
		n.Style = yaml.LiteralStyle
	case yaml11Bool[strings.ToLower(s)]:
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

var yaml11Bool = map[string]bool{"y": true, "yes": true, "n": true, "no": true, "on": true, "off": true, "true": true, "false": true}

// jsonValue decodes JSON keeping numbers as written, for embedding a
// dashboard in a YAML object.
func jsonValue(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}
