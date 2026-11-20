package protocol

import (
	"strings"
	"testing"
)

func TestCleanPublish(t *testing.T) {
	big := `{"x":"` + strings.Repeat("a", PublishMaxBytes) + `"}`
	exact := `{"x":"` + strings.Repeat("a", PublishMaxBytes-8) + `"}`
	if len(exact) != PublishMaxBytes {
		t.Fatalf("test input is %d bytes", len(exact))
	}
	for _, c := range []struct {
		name, in, want string
	}{
		{"object", ` { "a" : 1 }` + "\n", `{"a":1}`},
		{"array", `[1, 2]`, `[1,2]`},
		{"string", `"hi"`, `"hi"`},
		{"at the cap", exact, exact},
		{"over the cap", big, ""},
		{"empty", ``, ""},
		{"blank", " \n", ""},
		{"not json", `{a:1}`, ""},
		{"cut off", `{"a":`, ""},
		{"two values", `{"a":1} {"b":2}`, ""},
		{"trailing text", `{"a":1} x`, ""},
		{"null", `null`, ""},
	} {
		got, ok := CleanPublish([]byte(c.in))
		if ok != (c.want != "") || string(got) != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, ok, c.want)
		}
	}
}

func TestPublishPathOK(t *testing.T) {
	for p, want := range map[string]bool{
		"out.json": true, "a/b.json": true, ".zm/publish.json": true,
		"": false, ".": false, "..": false, "../x": false, "/etc/passwd": false, "a/../b": false, "a//b": false,
		"a/": false, "a\nb": false, "a\x00b": false, strings.Repeat("a", 201): false,
	} {
		if got := PublishPathOK(p); got != want {
			t.Errorf("PublishPathOK(%q) = %v, want %v", p, got, want)
		}
	}
}
