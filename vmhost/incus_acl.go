package vmhost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	kitconfig "github.com/sengirunners/potluck/kit/config"
)

// An isolated Incus VM sends no mail (zm-eakh.12.5.13): before boot, and
// again once it runs, every NIC's security.acls must hold an egress rule that
// drops or rejects tcp to mailPorts, from and to anywhere. Incus applies drop
// and reject rules before any allow, whatever their order, so one such rule
// holds whatever else the owner's ACL allows. zm reads the ACLs through its
// restricted certificate; the owner writes them (README).

// incusACLRule is every field of Incus's NetworkACLRule. A rule is decoded
// strictly: one with a field this does not model never counts as a block
// (fail closed), and neither does one narrowed by source_port or ICMP.
type incusACLRule struct {
	Action          string `json:"action"`
	State           string `json:"state"`
	Description     string `json:"description"`
	Source          string `json:"source"`
	Destination     string `json:"destination"`
	Protocol        string `json:"protocol"`
	SourcePort      string `json:"source_port"`
	DestinationPort string `json:"destination_port"`
	ICMPType        string `json:"icmp_type"`
	ICMPCode        string `json:"icmp_code"`
	unmodelled      bool
}

// decodeACLRules decodes an ACL's egress rules, marking each one that holds
// a field incusACLRule does not model.
func decodeACLRules(raw []json.RawMessage) []incusACLRule {
	rules := make([]incusACLRule, len(raw))
	for i, b := range raw {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rules[i]); err != nil {
			rules[i] = incusACLRule{unmodelled: true}
		}
	}
	return rules
}

// mailBlocked answers the mail ports no drop or reject rule in these ACLs
// blocks.
func mailBlocked(rules []incusACLRule) []int {
	var open []int
	for _, p := range mailPorts {
		if !slices.ContainsFunc(rules, func(r incusACLRule) bool { return r.blocks(p) }) {
			open = append(open, p)
		}
	}
	return open
}

// blocks reports a rule that drops or rejects tcp to port from and to anywhere.
func (r incusACLRule) blocks(port int) bool {
	if r.unmodelled || r.Action != "drop" && r.Action != "reject" || r.State == "disabled" || r.Protocol != "tcp" ||
		r.Source != "" || r.SourcePort != "" || r.ICMPType != "" || r.ICMPCode != "" {
		return false
	}
	if d := strings.Split(strings.ReplaceAll(r.Destination, " ", ""), ","); r.Destination != "" &&
		!(slices.Contains(d, "0.0.0.0/0") && slices.Contains(d, "::/0")) {
		return false
	}
	if r.DestinationPort == "" {
		return true
	}
	for _, s := range strings.Split(r.DestinationPort, ",") {
		lo, hi, ok := strings.Cut(strings.TrimSpace(s), "-")
		a, err1 := strconv.Atoi(lo)
		b, err2 := a, error(nil)
		if ok {
			b, err2 = strconv.Atoi(hi)
		}
		if err1 == nil && err2 == nil && a <= port && port <= b {
			return true
		}
	}
	return false
}

// mailFenced refuses a VM one of whose NICs lets mail out.
func (l incusCLI) mailFenced(in incusInstance) error {
	names := make([]string, 0, len(in.ExpandedDevices))
	for n, d := range in.ExpandedDevices {
		if d["type"] == "nic" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var bad []string
	for _, n := range names {
		var rules []incusACLRule
		for _, acl := range strings.Split(in.ExpandedDevices[n]["security.acls"], ",") {
			acl = strings.TrimSpace(acl)
			if !incusName.MatchString(acl) {
				return fmt.Errorf("nic %s: ACL %q is not a plain Incus name", n, acl)
			}
			out, err := l.run("query", l.apiPath("/1.0/network-acls/"+acl+"?project="+l.project))
			if err != nil {
				return fmt.Errorf("nic %s: %s cannot read its ACL %s to check it blocks outbound mail: %v", n, kitconfig.Program, acl, err)
			}
			var a struct {
				Egress []json.RawMessage `json:"egress"`
			}
			if err := json.Unmarshal([]byte(out), &a); err != nil {
				return fmt.Errorf("network ACL %s: %v", acl, err)
			}
			rules = append(rules, decodeACLRules(a.Egress)...)
		}
		if open := mailBlocked(rules); len(open) > 0 {
			bad = append(bad, fmt.Sprintf("nic %s lets outbound mail out (tcp %v): add an egress rule to its ACL: incus network acl rule add <acl> egress action=reject protocol=tcp destination_port=25,465,587", n, open))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("the VM is not fenced: %s", strings.Join(bad, "; "))
	}
	return nil
}
