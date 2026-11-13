package jobrule

import "testing"

func TestCheck(t *testing.T) {
	ok := Job{Repo: "acme/app", HeadRepo: "acme/app", RepoID: 9, HeadRepoID: 9, Workflow: ".github/workflows/lander.yml", Ref: "refs/heads/main",
		DefaultBranch: "main", Event: "push", RunID: 9}
	if why := Check(ok); why != "" {
		t.Fatalf("a main push: %s", why)
	}
	for name, mod := range map[string]func(*Job){
		"a fork":                     func(j *Job) { j.HeadRepo = "stranger/app" },
		"no head repository":         func(j *Job) { j.HeadRepo = "" },
		"a fork under the same name": func(j *Job) { j.HeadRepoID = 10 },
		"no repository id":           func(j *Job) { j.RepoID, j.HeadRepoID = 0, 0 },
		"no head repository id":      func(j *Job) { j.HeadRepoID = 0 },
		"no repository":              func(j *Job) { j.Repo, j.HeadRepo = "", "" },
		"no run id":                  func(j *Job) { j.RunID = 0 },
		"pull_request":               func(j *Job) { j.Event = "pull_request" },
		"pull_request_target":        func(j *Job) { j.Event = "pull_request_target" },
		"workflow_run":               func(j *Job) { j.Event = "workflow_run" },
		"merge_group":                func(j *Job) { j.Event = "merge_group" },
		"release":                    func(j *Job) { j.Event = "release" },
		"another branch":             func(j *Job) { j.Ref = "refs/heads/dev" },
		"a tag named main":           func(j *Job) { j.Ref = "refs/tags/main" },
		"no default branch":          func(j *Job) { j.DefaultBranch = "" },
		"a workflow outside":         func(j *Job) { j.Workflow = "ci.yml" },
		"a workflow escaping":        func(j *Job) { j.Workflow = ".github/workflows/../x.yml" },
	} {
		j := ok
		mod(&j)
		if Check(j) == "" {
			t.Errorf("%s: passed", name)
		}
	}
}

func TestRuleEvents(t *testing.T) {
	evs, err := RuleEvents(nil)
	if err != nil || len(evs) != 2 {
		t.Fatalf("default: %v %v", evs, err)
	}
	j := Job{Event: "workflow_dispatch", Workflow: ".github/workflows/a.yml"}
	if Serves(evs, nil, j) {
		t.Error("the default serves workflow_dispatch")
	}
	evs, _ = RuleEvents([]string{"workflow_dispatch"})
	if !Serves(evs, nil, j) || Serves(evs, []string{".github/workflows/b.yml"}, j) {
		t.Error("an opted-in dispatch, or the workflow list")
	}
	for _, bad := range []string{"merge_group", "release", "pull_request", "workflow_run", ""} {
		if _, err := RuleEvents([]string{bad}); err == nil {
			t.Errorf("event %q accepted", bad)
		}
	}
}
