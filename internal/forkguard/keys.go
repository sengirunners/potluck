package forkguard

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Key is how a mapping key written in a workflow is compared with a keyword (on, jobs, runs-on, a job id, ...):
// without regard to case, and with true the same as on, since YAML 1.1 readers decode an unquoted on as true. Every
// reader of workflow keys here (Apply, the fork-safe lint, potluck ci doctor) compares through Key, so a file cannot
// hide a key from one of them by its spelling. Where GitHub reads a spelling differently, these readers see one key
// more than GitHub does, which only ever makes them refuse.
func Key(written string) string {
	k := strings.ToLower(written)
	if k == "true" {
		return "on"
	}
	return k
}

// written is the key a mapping key node names: an alias used as a key names its anchor's text, as GitHub reads it.
func written(k *yaml.Node) string {
	for k.Kind == yaml.AliasNode && k.Alias != nil {
		k = k.Alias
	}
	return k.Value
}

// Field is the value of key in mapping n, compared through Key; nil when absent or when n is not a mapping. A key
// written twice, in any spellings Key takes as one (an alias used as a key included), is an error.
func Field(n *yaml.Node, key string) (*yaml.Node, error) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, nil
	}
	want := Key(key)
	var v *yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		if Key(written(n.Content[i])) == want {
			if v != nil {
				return nil, fmt.Errorf("key %q is written twice", key)
			}
			v = n.Content[i+1]
		}
	}
	return v, nil
}

// Duplicate is the error of the first key of mapping n that is written twice (Field's rule applied to every key of
// it); nil for none, or when n is not a mapping.
func Duplicate(n *yaml.Node) error {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if _, err := Field(n, written(n.Content[i])); err != nil {
			return err
		}
	}
	return nil
}

// Triggers is the trigger names of the workflow doc, a decoded workflow mapping: its on as one name, a list, or a
// mapping's keys. A workflow with no on is an error, not a workflow without triggers: what can start it is unknown,
// so none of its jobs can be judged safe.
func Triggers(doc *yaml.Node) ([]string, error) {
	on, err := Field(doc, "on")
	if err != nil {
		return nil, err
	}
	if on == nil {
		return nil, fmt.Errorf("the workflow has no `on` key, so what can start it is unknown")
	}
	return eventNames(on), nil
}
