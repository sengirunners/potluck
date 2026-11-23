package vmhost

// What a VM may take of its host beyond its own size (docs/cloudhv.md,
// "Limits"). A guest's processors and memory are the VM's size, and its
// disk's size is its image's. How fast it reads and writes its disk and how
// fast it sends and receives are not: unlimited, one guest fills the host's disk
// queue and its link, and its hypervisor takes more of the host's
// processors than the VM has. The hypervisor holds each to a rate itself.

import "strconv"

// cloudhvLimits are a binding's rates for each of its VMs. 0 is no limit.
type cloudhvLimits struct {
	// DiskMBs is how many MiB a second the guest reads and writes of its
	// disk, DiskOps how many requests a second.
	DiskMBs int `json:"disk_mb_s,omitempty"`
	DiskOps int `json:"disk_ops_s,omitempty"`
	// NetMbit is how many megabits a second the guest sends, and how many
	// it receives.
	NetMbit int `json:"net_mbit_s,omitempty"`
}

// cloudhvRateKey is one of a binding's rates: its driver key, where the
// rate is kept, and the most it can be.
type cloudhvRateKey struct {
	key string
	n   *int
	max int
}

// keys are the binding's driver keys for l's rates.
func (l *cloudhvLimits) keys() []cloudhvRateKey {
	return []cloudhvRateKey{{"ch_disk_mb_s", &l.DiskMBs, 100000}, {"ch_disk_ops_s", &l.DiskOps, 10000000}, {"ch_net_mbit_s", &l.NetMbit, 100000}}
}

// cloudhvRate is one of the hypervisor's rate limits: a bucket that holds
// what perSecond gives in one second, and fills at that rate. kind is bw
// (bytes) or ops (requests).
func cloudhvRate(kind string, perSecond int64) string {
	if perSecond <= 0 {
		return ""
	}
	return "," + kind + "_size=" + strconv.FormatInt(perSecond, 10) + "," + kind + "_refill_time=1000"
}

// disk is what the limits add to the hypervisor's --disk.
func (l cloudhvLimits) disk() string {
	return cloudhvRate("bw", int64(l.DiskMBs)<<20) + cloudhvRate("ops", int64(l.DiskOps))
}

// net is what the limits add to the hypervisor's --net.
func (l cloudhvLimits) net() string {
	return cloudhvRate("bw", int64(l.NetMbit)*125000)
}

// cloudhvRatesDiffer says why a warm box made under the driver keys made is
// not one its binding makes now, "" when it is: it is held to the rates of
// its boot, and handed out it would run at rates the binding no longer
// gives, or at none.
func cloudhvRatesDiffer(made map[string]string, now config) string {
	var was cloudhvLimits
	for _, k := range was.keys() {
		*k.n, _ = strconv.Atoi(made[k.key])
	}
	if was != now.CloudHV.Limits {
		return "the binding's rates changed since it booted"
	}
	return ""
}
