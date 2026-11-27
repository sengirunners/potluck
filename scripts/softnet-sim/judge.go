package main

import (
	"fmt"
	"slices"
	"strings"
)

// want is what Potluck's rules must do with a probe.
type want int

const (
	stopped  want = iota // must not leave Softnet
	answered             // must leave and be answered
	parity               // no promise: in the report, for comparing two Softnets
)

// control is what shows that a stopped probe would have arrived.
type control int

const (
	controlAnswer control = iota // with no rules in the way it left and was answered
	controlOut                   // with no rules in the way it left (nothing answers such a frame)
	controlNone                  // Softnet stops it under every rule set: only a Softnet that filters nothing lets it out
)

type probe struct {
	group, name string
	frame       []byte
	want        want
	control     control
	reply       func(seen) bool
}

// result is what became of one probe.
type result struct {
	Group    string `json:"group"`
	Name     string `json:"name"`
	Out      bool   `json:"left_softnet"`
	Answered bool   `json:"answered"`
}

// ruleSet is one set of Softnet rules to run every probe under.
type ruleSet struct {
	Name  string `json:"name"`
	Block string `json:"block"`
	Allow string `json:"allow"`
	// must are the groups a broken set must fail, or it is not caught.
	must []string
	// mutant runs the Softnet that filters nothing (-mutant).
	mutant bool
}

// runReport is what one rule set did.
type runReport struct {
	ruleSet
	Started     string   `json:"started,omitempty"` // why it did not, when it did not
	VM          string   `json:"vm,omitempty"`
	Gateway     string   `json:"gateway,omitempty"`
	DNS         []string `json:"dns,omitempty"`
	Process     []string `json:"softnet_uids,omitempty"` // effective, real, saved
	HTTPS       string   `json:"https,omitempty"`
	Ended       string   `json:"ended,omitempty"`
	InboundIPv6 string   `json:"inbound_ipv6,omitempty"`
	// Apart is true when macOS gave the two VMs a network each: no frame
	// of one can reach the other's, so nothing shows the peer checks would
	// fail.
	Apart bool `json:"vms_apart,omitempty"`
	// Harness is what went wrong in this program, not in Softnet: with any,
	// the run shows nothing.
	Harness  []string `json:"harness_problems,omitempty"`
	Results  []result `json:"results"`
	Failures []string `json:"failures"` // against Potluck's promise
	Verdict  string   `json:"verdict"`
}

type report struct {
	Label        string      `json:"label"`
	MacOS        string      `json:"macos"`
	Build        string      `json:"build"`
	VMMPres      string      `json:"kern_hv_vmm_present"`
	HVSupport    string      `json:"kern_hv_support"`
	Softnet      string      `json:"softnet_version"`
	SHA256       string      `json:"softnet_sha256"`
	MutantSHA256 string      `json:"mutant_sha256,omitempty"`
	Notes        []string    `json:"notes"`
	HostAddrs    []string    `json:"host_addresses"`
	Runs         []runReport `json:"runs"`
	// Unproven is what this run did not show, whatever the verdicts say.
	Unproven []string `json:"unproven,omitempty"`
	Pass     bool     `json:"pass"`
}

// openRules is the rule set with nothing in the way.
const openRules = "open"

// controls are what show that a stopped probe would have arrived: the open
// run's results and the filter-nothing Softnet's, by key.
type controls struct{ open, mutant map[string]result }

func byKey(rs []result) map[string]result {
	m := map[string]result{}
	for _, x := range rs {
		m[x.Group+" "+x.Name] = x
	}
	return m
}

// judge lists what breaks Potluck's promise in r. ctl nil leaves out whether
// a stopped probe proves anything (a deliberately broken rule set). wants are
// the probes by group and name.
func judge(r runReport, wants map[string]probe, ctl *controls) (fails []string) {
	fails = []string{}
	if r.Started != "" {
		return []string{"start: " + r.Started}
	}
	for _, h := range r.Harness {
		fails = append(fails, "harness: "+h)
	}
	for _, x := range r.Results {
		key := x.Group + " " + x.Name
		p, ok := wants[key]
		if !ok {
			fails = append(fails, "harness: no probe is named "+key)
			continue
		}
		switch p.want {
		case answered:
			if !x.Out || !x.Answered {
				fails = append(fails, fmt.Sprintf("%s: must work (left Softnet: %v, answered: %v)", key, x.Out, x.Answered))
			}
		case stopped:
			if x.Out || x.Answered {
				fails = append(fails, key+": must be stopped and was not")
			} else if ctl != nil && !(p.group == "peer" && r.Apart) {
				if why := ctl.unproven(key, p.control); why != "" {
					fails = append(fails, key+": stopped, but "+provesNothing+": "+why)
				}
			}
		}
	}
	if !r.mutant && !strings.HasPrefix(r.HTTPS, "ok") {
		fails = append(fails, "internet https: "+r.HTTPS)
	}
	if !r.mutant && (len(r.Process) != 3 || slices.Contains(r.Process, "0")) {
		fails = append(fails, fmt.Sprintf("softnet uids: root kept, or not read: %v", r.Process))
	}
	if !strings.HasPrefix(r.Ended, "on SIGINT, exit") {
		fails = append(fails, "softnet end: "+r.Ended)
	}
	return fails
}

const provesNothing = "proves nothing"

// unproven is why a stopped probe's silence shows nothing ("" when it does).
func (c *controls) unproven(key string, ctl control) string {
	if ctl == controlNone {
		if c.mutant == nil { // the whole run says so
			return ""
		}
		if !c.mutant[key].Out {
			return "a Softnet that filters nothing did not let it out either"
		}
		return ""
	}
	if o := c.open[key]; !o.Out || ctl == controlAnswer && !o.Answered {
		return fmt.Sprintf("with no rules in the way it left %v, was answered %v", o.Out, o.Answered)
	}
	return ""
}

// groupsOf counts, by group, what a rule set let out or held back wrongly.
func groupsOf(fails []string) map[string]int {
	g := map[string]int{}
	for _, f := range fails {
		if !strings.Contains(f, provesNothing) {
			g[strings.SplitN(f, " ", 2)[0]]++
		}
	}
	return g
}

// verdicts fills in each run's verdict and the report's: Pass only when
// every check ran, every control showed it would fail, and none did.
func (rep *report) verdicts(wants map[string]probe) {
	ctl := controls{open: map[string]result{}}
	for _, r := range rep.Runs {
		if r.Started != "" || len(r.Harness) > 0 {
			continue
		}
		switch {
		case r.Name == openRules:
			ctl.open = byKey(r.Results)
		case r.mutant:
			ctl.mutant = byKey(r.Results)
		}
	}
	if ctl.mutant == nil {
		rep.Unproven = append(rep.Unproven, "no Softnet that filters nothing ran: nothing shows the checks of what every rule set stops would fail")
	}
	rep.Pass = true
	for i := range rep.Runs {
		r := &rep.Runs[i]
		switch {
		case r.Name == openRules:
			r.Failures, r.Verdict = append([]string{}, r.Harness...), "control: what the endpoints answer with no rules in the way"
			if r.Started != "" || len(r.Harness) > 0 {
				r.Verdict, rep.Pass = "FAIL: the control did not run: "+r.Started+strings.Join(r.Harness, "; "), false
			}
		case len(r.must) == 0:
			r.Failures, r.Verdict = judge(*r, wants, &ctl), "PASS"
			if len(r.Failures) > 0 {
				r.Verdict, rep.Pass = fmt.Sprintf("FAIL: %d", len(r.Failures)), false
			}
			if r.Apart {
				rep.Unproven = append(rep.Unproven, "macOS gave each VM a network of its own under "+r.Name+", so its peer checks show nothing")
			}
		default:
			r.Failures = judge(*r, wants, nil)
			g := groupsOf(r.Failures)
			var missed []string
			for _, m := range r.must {
				if g[m] == 0 && !(m == "peer" && r.Apart) {
					missed = append(missed, m)
				}
			}
			r.Verdict = "caught"
			if r.Started != "" || len(r.Harness) > 0 || len(missed) > 0 {
				r.Verdict, rep.Pass = "NOT CAUGHT: "+strings.Join(missed, ", ")+r.Started+strings.Join(r.Harness, "; "), false
			}
		}
	}
	rep.Pass = rep.Pass && len(rep.Runs) > 0 && len(rep.Unproven) == 0
}
