package main

import (
	"strings"
	"testing"
)

// The judge is what says PASS, so each way a run can look right and show
// nothing must not pass.
func TestVerdicts(t *testing.T) {
	wants := map[string]probe{
		"dns name":      {group: "dns", want: answered},
		"host tcp 22":   {group: "host", want: stopped, control: controlAnswer},
		"special udp":   {group: "special", want: stopped, control: controlOut},
		"spoof arp":     {group: "spoof", want: stopped, control: controlNone},
		"peer direct":   {group: "peer", want: stopped, control: controlOut},
		"parity ip 253": {group: "parity", want: parity},
	}
	res := func(dns, host, special, spoof, peer bool) []result {
		return []result{{"dns", "name", dns, dns}, {"host", "tcp 22", host, host}, {"special", "udp", special, false},
			{"spoof", "arp", spoof, false}, {"peer", "direct", peer, false}, {"parity", "ip 253", true, false}}
	}
	run := func(rs ruleSet, r []result) runReport {
		return runReport{ruleSet: rs, Results: r, HTTPS: "ok: 200", Ended: "on SIGINT, exit status 0", Process: []string{"501", "501", "501"}}
	}
	// 0 potluck, 1 open, 2 a broken set, 3 the Softnet that filters nothing.
	good := func() *report {
		return &report{Runs: []runReport{
			run(ruleSet{Name: "potluck"}, res(true, false, false, false, false)),
			run(ruleSet{Name: openRules}, res(true, true, true, false, true)),
			run(ruleSet{Name: "broken", must: []string{"host", "peer"}}, res(true, true, false, false, true)),
			run(ruleSet{Name: "mutant", mutant: true, must: []string{"spoof"}}, res(true, true, true, true, true)),
		}}
	}
	for _, tt := range []struct {
		name string
		harm func(*report)
		want string // in the verdicts, failures and what was not shown
	}{
		{"a leak", func(r *report) { r.Runs[0].Results[1].Out = true }, "host tcp 22: must be stopped and was not"},
		{"an answer to what was not seen to leave", func(r *report) { r.Runs[0].Results[1].Answered = true }, "host tcp 22: must be stopped and was not"},
		{"a promised answer missing", func(r *report) { r.Runs[0].Results[0].Answered = false }, "dns name: must work"},
		{"a control endpoint that never answers", func(r *report) { r.Runs[1].Results[1].Answered = false }, "host tcp 22: stopped, but proves nothing"},
		{"a control frame that never leaves", func(r *report) { r.Runs[1].Results[2].Out = false }, "special udp: stopped, but proves nothing"},
		{"a frame even no filter lets out", func(r *report) {
			r.Runs[3].Results[3].Out = false
			r.Runs[3].Results = append(r.Runs[3].Results, result{"spoof", "other", true, false})
		}, "spoof arp: stopped, but proves nothing"},
		{"no Softnet that filters nothing", func(r *report) { r.Runs = r.Runs[:3] }, "no Softnet that filters nothing ran"},
		{"no open run", func(r *report) { r.Runs = append(r.Runs[:1], r.Runs[2:]...) }, "host tcp 22: stopped, but proves nothing"},
		{"an open run that missed frames", func(r *report) { r.Runs[1].Harness = []string{"frames dropped"} }, "the control did not run"},
		{"frames missed", func(r *report) { r.Runs[0].Harness = []string{"frames dropped"} }, "harness: frames dropped"},
		{"a broken set that fails elsewhere", func(r *report) { r.Runs[2].Results[1].Out, r.Runs[2].Results[1].Answered = false, false }, "NOT CAUGHT: host"},
		{"a broken set whose second VM did not start", func(r *report) { r.Runs[2].Harness = []string{"the second VM did not start"} }, "NOT CAUGHT: the second VM did not start"},
		{"a broken set that did not start", func(r *report) { r.Runs[2].Started = "no interface" }, "NOT CAUGHT: "},
		{"a VM each on its own network", func(r *report) { r.Runs[0].Apart = true }, "its peer checks show nothing"},
		{"a result no probe is named for", func(r *report) { r.Runs[0].Results = append(r.Runs[0].Results, result{"host", "new", false, false}) }, "no probe is named host new"},
		{"no HTTPS", func(r *report) { r.Runs[0].HTTPS = "failed: timeout" }, "internet https"},
		{"softnet that dies", func(r *report) { r.Runs[0].Ended = "softnet ended by itself" }, "softnet end"},
		{"softnet that keeps root", func(r *report) { r.Runs[0].Process[2] = "0" }, "softnet uids"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := good()
			if r.verdicts(wants); !r.Pass {
				t.Fatalf("the run to break does not pass: %+v", r)
			}
			r = good()
			tt.harm(r)
			r.verdicts(wants)
			all := strings.Join(r.Unproven, "\n")
			for _, x := range r.Runs {
				all += "\n" + x.Verdict + "\n" + strings.Join(x.Failures, "\n")
			}
			if r.Pass || !strings.Contains(all, tt.want) {
				t.Fatalf("pass %v, without %q:\n%s", r.Pass, tt.want, all)
			}
		})
	}
}
