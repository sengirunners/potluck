package forkguard

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// MacJobs is the ids of the jobs in workflow file src that run on a GitHub-hosted macOS label, in file order: a job
// whose runs-on is one such label (a scalar, or a one-element list), or already the guarded value (Parse) that keeps
// a fork's pull request on one. potluck ci pr uses it to pick the job when it is not named. A file that does not
// decode, or whose jobs cannot be read, has none; Apply says why when a job of it is named.
func MacJobs(src []byte) []string {
	doc, err := oneDocument(src)
	if err != nil {
		return nil
	}
	jobs, err := Field(doc, "jobs")
	if err != nil || jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	var out []string
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		j := jobs.Content[i+1]
		ro, err := Field(j, "runs-on")
		if err != nil || ro == nil {
			continue
		}
		if ro.Kind == yaml.SequenceNode && len(ro.Content) == 1 {
			ro = ro.Content[0]
		}
		if ro.Kind != yaml.ScalarNode {
			continue
		}
		label := ro.Value
		if g, ok := Parse(label); ok {
			label = g.Fork
		}
		if strings.HasPrefix(label, "macos-") && Hosted(label) {
			out = append(out, written(jobs.Content[i]))
		}
	}
	return out
}
