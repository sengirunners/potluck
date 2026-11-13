package hostprobe

import (
	"slices"
	"strings"
)

// dockerRuntime derives Docker readiness from the owner's explicit runtime.
// Doctor's default preference and readiness remain facts about that default.
// A registered Kata runtime honours setuid in its guest; it does not need the
// default gVisor runtime's runsc-root installation. Its KVM check still applies.
func (o ShareOptions) dockerRuntime(l Layer) Layer {
	on, _ := o.docker()
	rt := o.OCIRuntime
	if l.Name != "oci" || !on || rt == "" || !IsolatingRuntime(rt) || !strings.Contains(rt, "kata") {
		return l
	}
	l.Sudo, l.SudoProblem, l.SudoSteps = "", "", nil
	switch {
	case !slices.Contains(l.Runtimes, rt):
		l.SudoProblem = "requested runtime " + rt + " is not registered with " + l.CLI
	case !o.Report.KVM:
		l.SudoProblem = "requested runtime " + rt + " needs usable /dev/kvm: " + o.Report.KVMProblem
	default:
		l.Sudo = rt
	}
	return l
}
