package hostprobe

// How long a box may live (zm-eakh.12.47.10.10.1): potluck host --ttl-max-s N
// binds ttl_max_s=N on every size, and a re-run without the flag keeps an
// earlier binding's, as it keeps linux_slots (slots.go). The daemon sends a
// binding's ttl_max_s to its driver as config (runtarget driverConfig), so N
// may raise the driver's own default (potluck-driver-tart: 4 h) as well as
// lower it. Unset, the driver's default holds.

import (
	"fmt"
	"strconv"
)

// MinTTLMaxS and MaxTTLMaxS bound --ttl-max-s. The floor is 15 minutes: a
// lower cap leaves a box too short a life for a boot plus any real job, and
// the runner broker adds 300 s of startup to every job's runtime. The
// ceiling is GitHub Actions' 5-day limit on a self-hosted runner's job
// (432000 s) plus an hour for that startup and the box's release: a cap
// past it serves no job and only lets a forgotten box hold a slot longer.
const (
	MinTTLMaxS = 900
	MaxTTLMaxS = 432000 + 3600
)

// ttlMaxS is the ttl_max_s a share binds on every size it binds (0: none,
// so the driver's default) and the earlier binding it was kept from ("": the
// flag, or nothing kept).
func ttlMaxS(o ShareOptions, sizes []Size) (int, string, error) {
	if o.TTLMaxS != nil {
		n := *o.TTLMaxS
		if n < MinTTLMaxS || n > MaxTTLMaxS {
			return 0, "", fmt.Errorf("--ttl-max-s: want %d..%d seconds (15 minutes to GitHub's 5-day job limit plus an hour)", MinTTLMaxS, MaxTTLMaxS)
		}
		return n, "", ttlAlike(o, n, sizes)
	}
	if o.EarlierUnread != "" && !o.DryRun { // a dry run binds nothing
		return 0, "", fmt.Errorf("could not read this machine's bindings (%s): a re-run would drop the box lifetime an earlier share bound: try again, or pass --ttl-max-s N", o.EarlierUnread)
	}
	n, from := 0, ""
	for _, t := range shareBindings(o) {
		k := t.TTLMaxS
		if k == 0 {
			continue
		}
		if k < MinTTLMaxS || k > MaxTTLMaxS {
			return 0, "", fmt.Errorf("binding %s has ttl_max_s=%d: pass --ttl-max-s N (%d..%d) to replace it", t.Name, k, MinTTLMaxS, MaxTTLMaxS)
		}
		if from != "" && k != n {
			return 0, "", fmt.Errorf("bindings %s and %s differ in ttl_max_s (%d and %d): pass --ttl-max-s N to say which", from, t.Name, n, k)
		}
		n, from = k, t.Name
	}
	return n, from, nil
}

// ttlAlike refuses a run that binds ttl_max_s n while a size of this share
// it leaves alone carries another one: every later run without the flag
// would be refused for bindings that differ. A size left alone with none
// is fine: a re-run keeps n.
func ttlAlike(o ShareOptions, n int, sizes []Size) error {
	bound := map[string]bool{}
	for _, s := range sizes {
		bound[o.NamePrefix+"-"+s.Name] = true
	}
	for _, t := range shareBindings(o) {
		if !bound[t.Name] && t.TTLMaxS != 0 && t.TTLMaxS != n {
			return fmt.Errorf("--ttl-max-s %d: binding %s is not in this run and lets boxes live %d s, and every size of one share carries the same cap: share it in this run too (--sizes), or give it the same number", n, t.Name, t.TTLMaxS)
		}
	}
	return nil
}

// ttlNote is a share's note for a bound ttl_max_s.
func ttlNote(n int, from string) string {
	return "boxes live at most " + strconv.Itoa(n) + " s (" + lifeWords(n) + ") on every size" + keptFrom(from)
}

// lifeWords is n seconds as hours, or days and hours.
func lifeWords(n int) string {
	h := n / 3600
	switch {
	case h < 1:
		return strconv.Itoa(n/60) + " minutes"
	case h < 48:
		return strconv.Itoa(h) + " h"
	}
	return fmt.Sprintf("%d days %d h", h/24, h%24)
}
