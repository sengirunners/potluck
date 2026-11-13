package forkguard

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func mapping(t *testing.T, src string) *yaml.Node {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(src), &root); err != nil {
		t.Fatal(err)
	}
	return root.Content[0]
}

// A key written twice is an error however it is spelled: in another letter case, or as an alias of a scalar that
// spells it. A key written once is found.
func TestFieldKeyWrittenTwice(t *testing.T) {
	for _, tt := range []struct {
		name, src string
		clash     bool
	}{
		{"once", "on: push\njobs: {}\n", false},
		{"another letter case", "on: push\nON: pull_request\n", true},
		{"true for on", "on: push\ntrue: pull_request\n", true},
		{"an alias as the second key", "on: push\nz: &k on\n*k : pull_request\n", true},
		{"an alias of another word", "on: push\nz: &k build\n*k : pull_request\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v, err := Field(mapping(t, tt.src), "on")
			if (err != nil) != tt.clash {
				t.Fatalf("Field: %v, %v (clash %v)", v, err, tt.clash)
			}
			if _, err := Triggers(mapping(t, tt.src)); (err != nil) != tt.clash {
				t.Fatalf("Triggers: %v (clash %v)", err, tt.clash)
			}
		})
	}
}

// Duplicate finds a key written twice anywhere in a mapping, including two
// aliases that name one keyword.
func TestDuplicate(t *testing.T) {
	for _, tt := range []struct {
		name, src string
		clash     bool
	}{
		{"none", "on: push\njobs: {}\n", false},
		{"two spellings", "runs-on: a\nRuns-On: b\n", true},
		{"two aliases of one keyword", "x: &k runs-on\ny: &l Runs-On\n*k : a\n*l : b\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := Duplicate(mapping(t, tt.src)); (err != nil) != tt.clash {
				t.Fatalf("Duplicate: %v (clash %v)", err, tt.clash)
			}
		})
	}
}
