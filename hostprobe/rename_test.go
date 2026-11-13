package hostprobe

import "testing"

// The default conformance suite is potluck-conformance when it is on PATH,
// else zm-run-conformance.
func TestDefaultSuiteNames(t *testing.T) {
	on := map[string]bool{}
	look := func(name string) (string, bool) { return "/bin/" + name, on[name] }
	if got := defaultSuite(look); got != "zm-run-conformance" {
		t.Fatalf("neither: %s", got)
	}
	on["potluck-conformance"] = true
	if got := defaultSuite(look); got != "potluck-conformance" {
		t.Fatalf("new name on PATH: %s", got)
	}
}
