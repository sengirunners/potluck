package runners

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/internal/forkguard"
	"gopkg.in/yaml.v3"
)

// doctorWorkflows is checks 8 to 11, and 12 for each job of the pool: the
// repository's workflow files at its default branch, read with gh. facts
// is nil when the repository could not be read; pool is nil when the
// machines could not be.
func (s *Service) doctorWorkflows(ctx context.Context, repo string, out *DoctorReport, facts *doctorRepo, pool *doctorPool) {
	var files []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	}
	if err := s.personRead(ctx, "/repos/"+repo+"/contents/.github/workflows", &files); err != nil {
		for n := 8; n <= 11; n++ {
			out.set(n, "warn", "unavailable: workflow files could not be read using gh", "check the repository path and gh access to its workflow files")
		}
		return
	}
	for n := 8; n <= 11; n++ {
		out.set(n, "pass", "Workflow checks passed.", "")
	}
	if facts == nil {
		out.set(10, "warn", "unavailable: the repository's visibility is needed to judge its events", "check gh authentication, repository access and GitHub connectivity")
	}
	count := 0
	for _, f := range files {
		if f.Type != "file" || !(strings.HasSuffix(f.Name, ".yml") || strings.HasSuffix(f.Name, ".yaml")) {
			continue
		}
		count++
		var file struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := s.personRead(ctx, "/repos/"+repo+"/contents/"+f.Path, &file); err != nil || file.Encoding != "base64" {
			for n := 8; n <= 11; n++ {
				out.set(n, "warn", "unavailable: "+f.Path+" could not be read", "check the repository path and gh access to its workflow files")
			}
			continue
		}
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
		if err != nil {
			out.set(8, "warn", "unavailable: workflow content was not valid base64", "read "+f.Path)
			workflowUnavailable(out, f.Path)
			continue
		}
		var root yaml.Node
		var parsed any
		parseErr := yaml.Unmarshal(data, &parsed)
		if parseErr == nil {
			parseErr = yaml.Unmarshal(data, &root)
		}
		if parseErr != nil {
			out.set(8, "fail", f.Path+": "+parseErr.Error()+" (check for a colon in an unquoted value)", "quote the value in "+f.Path)
			workflowUnavailable(out, f.Path)
			continue
		}
		if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
			out.set(8, "fail", f.Path+": workflow must be a YAML mapping", "fix "+f.Path)
			workflowUnavailable(out, f.Path)
			continue
		}
		doc := root.Content[0]
		if err := forkguard.Duplicate(doc); err != nil {
			out.set(8, "fail", f.Path+": "+err.Error()+": GitHub refuses the file", "write each key once in "+f.Path)
			workflowUnavailable(out, f.Path)
			continue
		}
		jobs := yamlField(doc, "jobs")
		if jobs == nil || jobs.Kind != yaml.MappingNode {
			out.set(8, "fail", f.Path+": jobs must be a mapping", "fix "+f.Path)
			workflowUnavailable(out, f.Path)
			continue
		}
		events, err := forkguard.Triggers(doc)
		if err != nil {
			out.set(8, "fail", f.Path+": "+err.Error(), "fix "+f.Path)
			workflowUnavailable(out, f.Path)
			continue
		}
		if err := forkguard.Duplicate(jobs); err != nil {
			out.set(8, "fail", f.Path+": jobs: "+err.Error()+": GitHub refuses the file", "write each job once in "+f.Path)
			workflowUnavailable(out, f.Path)
			continue
		}
		clash := false
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			if err := forkguard.Duplicate(jobs.Content[i+1]); err != nil {
				out.set(8, "fail", f.Path+": job "+jobs.Content[i].Value+": "+err.Error()+": GitHub refuses the file", "write each key once in "+f.Path)
				clash = true
			}
		}
		if clash {
			workflowUnavailable(out, f.Path)
			continue
		}
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			s.doctorJob(ctx, repo, f.Path, data, jobs.Content[i].Value, jobs.Content[i+1], events, out, facts, pool)
		}
	}
	if count == 0 {
		for n := 8; n <= 11; n++ {
			out.set(n, "warn", "No workflow files were found.", "add a workflow to .github/workflows")
		}
	}
}

// doctorJob is checks 9 to 12 for one job (key name) of the workflow file
// data at path, which runs on events.
func (s *Service) doctorJob(ctx context.Context, repo, path string, data []byte, name string, job *yaml.Node, events []string, out *DoctorReport, facts *doctorRepo, pool *doctorPool) {
	on := yamlField(job, "runs-on")
	if on == nil {
		return // a reusable workflow's job: its runs-on is in the workflow it calls
	}
	labelNode := on
	if on.Kind == yaml.MappingNode {
		labelNode = yamlField(on, "labels")
	}
	labels := yamlStrings(labelNode)
	// A fork-guarded runs-on (forkguard, the lint's own rule) is judged by the labels own runs ask for; a
	// fork's pull request takes the hosted label and never reaches the pool.
	guarded := false
	if on.Kind == yaml.ScalarNode {
		if g, ok := forkguard.Parse(on.Value); ok && g.Routed == "" {
			labels, guarded = g.Own, forkguard.Holds(events)
		}
	}
	if labelNode == nil || labelNode.Kind == yaml.AliasNode || strings.Contains(strings.Join(labels, " "), "${{") {
		for n := 10; n <= 11; n++ {
			out.set(n, "warn", path+": "+name+": dynamic runs-on prevents job checks", "inspect "+path)
		}
		out.set(9, "warn", path+": "+name+": dynamic runs-on needs inspection", "inspect "+path)
		return
	}
	w, ours, why := s.labels(labels)
	if !ours {
		// Another fleet's, or GitHub's own: a self-hosted job with only
		// GitHub's default labels is not this pool's either.
		if SelfHosted(labels) && !doctorDefaultLabels(labels) {
			out.set(9, "warn", fmt.Sprintf("%s:%d: job %s: labels may belong to another self-hosted fleet", path, on.Line, name), "inspect runs-on if this pool was intended")
		}
		return
	}
	prefix := fmt.Sprintf("%s:%d: job %s: ", path, on.Line, name)
	if why != "" {
		out.set(9, "fail", prefix+why, "edit runs-on in "+path)
	}

	// Check 11: the runtime the broker gives the job on each of its events,
	// by its own rule (workflowLimit) over this file.
	limit := 0
	display := name
	if n := yamlField(job, "name"); n != nil && n.Kind == yaml.ScalarNode {
		display = n.Value // GitHub's display name; workflowLimit judges a dynamic one
	}
	for _, e := range cmpEvents(events) {
		v := s.workflowLimit(ctx, doctorWorkflowFile(data), out.Installation, repo, 0, 0,
			&ghapp.Run{Event: e, Repo: repo, HeadRepo: repo, Path: path, HeadSHA: "default branch"}, &ghapp.Job{Name: display})
		limit = max(limit, v.seconds)
		if v.fallback != "" && yamlField(job, "timeout-minutes") != nil {
			out.set(11, "warn", fmt.Sprintf("%son %s: the broker gives it job_ttl_s (%ds), not its timeout-minutes: %s", prefix, cmp.Or(e, "unknown event"), v.seconds, v.fallback),
				"set runners.job_ttl_s to the longest such job's runtime, or write timeout-minutes as a whole number of minutes in "+path+" (docs/workflow-job-limits.md)")
		}
	}

	// Check 12 for this job: placement's answer for its labels and runtime.
	placed := true
	if pool != nil && why == "" {
		if names, reason, permanent := pool.fit(w, limit+startupAllowance); len(names) == 0 {
			status := "warn"
			if permanent {
				status, placed = "fail", false
			}
			out.set(12, status, prefix+reason, "lend a machine that fits to the pool (from that machine)")
		}
	}

	// Check 9: a job naming no OS runs on linux, as on GitHub's runners
	// (labels); that fails only when no machine could take it.
	if why == "" && !slices.ContainsFunc(labels, func(l string) bool { return slices.Contains(osLabels, strings.ToLower(l)) }) {
		status := "warn"
		if !placed {
			status = "fail"
		}
		out.set(9, status, prefix+"no OS is named, so the broker places it on "+w.os, "name the OS (macos, linux or windows) in runs-on in "+path)
	}

	// Check 10: the run events the broker refuses (trustReason) for a run
	// of the repository's own branches; outside pull requests are refused
	// unless forkPRs serves them.
	if facts == nil {
		return
	}
	var d delivery
	d.Repo.FullName, d.Repo.Private, d.Repo.Visibility = repo, facts.Private, facts.Visibility
	for _, e := range events {
		if e == "workflow_call" {
			continue // a reusable workflow: its run has its caller's event
		}
		if why := s.trustReason(ghapp.Run{Repo: repo, HeadRepo: repo, Event: e}, d); why != "" {
			out.set(10, "fail", prefix+"on "+e+": "+why, "remove "+e+" from on: in "+path+" or run this job on GitHub's runners")
		}
	}
	if !s.forkPRs(d) && slices.Contains(events, "pull_request") && !guarded {
		// Arbitrary GitHub expressions cannot be proven safe by substring
		// matching: only the canonical same-repository guard counts.
		const safe = "github.event.pull_request.head.repo.full_name == github.repository"
		guard := yamlField(job, "if")
		switch {
		case guard == nil:
			out.set(10, "fail", prefix+"no fork guard: an outside pull request's job is refused and waits", "add `if: "+safe+"` to the job in "+path)
		case strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(guard.Value), "${{"), "}}")) != safe:
			out.set(10, "warn", prefix+"fork guard cannot be proven; the broker still refuses outside pull requests", "add `if: "+safe+"` to the job in "+path)
		}
	}
}

// cmpEvents is events without repeats; no event at all is one unknown event.
func cmpEvents(events []string) []string {
	if len(events) == 0 {
		return []string{""}
	}
	return slices.Compact(slices.Sorted(slices.Values(events)))
}

// doctorWorkflowFile is a workflow file the doctor read with gh, as the
// source the broker's workflowLimit reads; the doctor names the run and
// the job itself.
type doctorWorkflowFile []byte

func (f doctorWorkflowFile) WorkflowFile(context.Context, int64, string, string, string) ([]byte, error) {
	return f, nil
}

func (doctorWorkflowFile) Run(context.Context, int64, string, int64) (ghapp.Run, error) {
	return ghapp.Run{}, errors.New("the doctor reads no run")
}

func (doctorWorkflowFile) Job(context.Context, int64, string, int64) (ghapp.Job, error) {
	return ghapp.Job{}, errors.New("the doctor reads no job")
}

// yamlField is forkguard.Field (GitHub's key matching); a key written twice reads as absent, but doctorWorkflows
// fails check 8 on those (forkguard.Duplicate) before any job is read.
func yamlField(n *yaml.Node, key string) *yaml.Node {
	v, err := forkguard.Field(n, key)
	if err != nil {
		return nil
	}
	return v
}

func yamlStrings(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		for _, v := range n.Content {
			out = append(out, v.Value)
		}
		return out
	case yaml.MappingNode:
		var out []string
		for i := 0; i+1 < len(n.Content); i += 2 {
			out = append(out, n.Content[i].Value)
		}
		return out
	}
	return nil
}

func workflowUnavailable(out *DoctorReport, path string) {
	for n := 9; n <= 11; n++ {
		out.set(n, "warn", "unavailable: "+path+" could not be checked", "fix "+path)
	}
}

// SelfHosted reports whether labels name a self-hosted runner, as runs-on's
// "self-hosted" label does (GitHub reads it without regard to case).
func SelfHosted(labels []string) bool {
	return slices.ContainsFunc(labels, func(l string) bool { return strings.EqualFold(l, "self-hosted") })
}

// doctorDefaultLabels: GitHub gives every self-hosted runner these labels;
// a job naming only them does not ask for this pool.
func doctorDefaultLabels(labels []string) bool {
	for _, label := range labels {
		l := strings.ToLower(label)
		if l != "self-hosted" && !slices.Contains(osLabels, l) && !slices.Contains(archLabels, l) {
			return false
		}
	}
	return true
}
