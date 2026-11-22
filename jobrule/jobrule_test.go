package jobrule

import (
	"strings"
	"testing"
)

func TestAllowedEvents(t *testing.T) {
	for _, event := range []string{"push", "schedule", "workflow_dispatch", "repository_dispatch"} {
		if !Allowed(event) {
			t.Errorf("expected %q to be allowed", event)
		}
	}
	for _, event := range []string{"pull_request", "pull_request_target", "workflow_run", "merge_group", "release", "issues", ""} {
		if Allowed(event) {
			t.Errorf("expected %q to be refused", event)
		}
	}
}

func TestValidRepo(t *testing.T) {
	for _, repo := range []string{"acme/app", "a.b/c_d-e"} {
		if !ValidRepo(repo) {
			t.Errorf("expected %q to be valid", repo)
		}
	}
	for _, repo := range []string{"acme", "acme/", "/app", "a/b/c", "acme/ap p", "a..b/c", "", strings.Repeat("a", 101) + "/app"} {
		if ValidRepo(repo) {
			t.Errorf("expected %q to be invalid", repo)
		}
	}
}

func TestValidWorkflow(t *testing.T) {
	for _, path := range []string{".github/workflows/ci.yml", ".github/workflows/ci.yaml"} {
		if !ValidWorkflow(path) {
			t.Errorf("expected %q to be valid", path)
		}
	}
	for _, path := range []string{"ci.yml", ".github/workflows/sub/ci.yml", ".github/workflows/../x.yml", ".github/workflows/ci.txt", ".github/workflows/.yml"} {
		if ValidWorkflow(path) {
			t.Errorf("expected %q to be invalid", path)
		}
	}
}
