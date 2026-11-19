package hostprobe

// How many Linux boxes a share runs at once (zm-eakh.12.7.11): potluck host
// --linux-slots N binds zm-run-tart's linux_slots=N on every size, and a
// re-run without the flag keeps an earlier binding's, as it keeps admission
// keys. The driver counts every live box of its state directory against the
// acquiring binding's slots, whichever size made it, so:
//
//   - without an admission controller, every size lent must fit N times
//     inside the owner's share (Limits.Boxes): N boxes of the largest size
//     may run together;
//   - with admission_url bound, the controller decides room for each box
//     (and fails closed: no lease, no box), so N is only the owner's
//     ceiling and the sizes fit Slots times, as without the flag;
//   - every size of the share carries the same N, or one binding would
//     refuse what another takes.
//
// macOS guests stay at macos_slots, which Apple's licence caps at 2 per Mac.
// Conformance runs with linux_slots max(N, Slots): its own boxes count
// against the same slots as the live ones, and its peers check runs two.

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/conformance"
	"github.com/sengirunners/potluck/vmhost"
)

// MaxLinuxSlots bounds --linux-slots.
const MaxLinuxSlots = 256

// linuxSlots is the linux_slots a share binds on every size (0: none, so
// the driver's default, Slots) and the earlier binding it was kept from
// ("": the flag, or nothing kept). macOS says the images are macOS guests,
// which the flag cannot raise.
func linuxSlots(o ShareOptions, macOS bool) (int, string, error) {
	if o.LinuxSlots != nil {
		n := *o.LinuxSlots
		switch {
		case n < 1 || n > MaxLinuxSlots:
			return 0, "", fmt.Errorf("--linux-slots: want 1..%d Linux boxes at once", MaxLinuxSlots)
		case macOS:
			return 0, "", fmt.Errorf("--linux-slots is for Linux boxes: these images are macOS, which Apple's licence caps at %d per Mac", Slots)
		}
		return n, "", nil
	}
	if macOS {
		return 0, "", nil
	}
	if o.EarlierUnread != "" && !o.DryRun { // a dry run binds nothing
		return 0, "", fmt.Errorf("could not read this machine's bindings (%s): a re-run would drop the Linux slots an earlier share bound: try again, or pass --linux-slots N and share every size (--sizes %s)", o.EarlierUnread, ladderNames())
	}
	n, from := 0, ""
	for _, t := range shareBindings(o) {
		v := t.Config["linux_slots"]
		if v == "" || oneAtATime(t) { // one at a time is each run's own finding, not the owner's choice
			continue
		}
		k, err := strconv.Atoi(v)
		if err != nil || k < 1 || k > MaxLinuxSlots {
			return 0, "", fmt.Errorf("binding %s has linux_slots=%s: pass --linux-slots N (1..%d) to replace it", t.Name, v, MaxLinuxSlots)
		}
		if from != "" && k != n {
			return 0, "", fmt.Errorf("bindings %s and %s differ in linux_slots (%d and %d): pass --linux-slots N to say which", from, t.Name, n, k)
		}
		n, from = k, t.Name
	}
	return n, from, nil
}

// slotsAlike refuses a run that binds linux_slots n while a size of this
// share it leaves alone runs another number: the driver counts every live
// box against the acquiring binding's own slots, so the larger number would
// let boxes of the other size past its fit.
func slotsAlike(o ShareOptions, n int, from string, sizes []Size) error {
	if n == 0 {
		return nil
	}
	bound := map[string]bool{}
	for _, s := range sizes {
		bound[o.NamePrefix+"-"+s.Name] = true
	}
	if o.EarlierUnread != "" && !o.DryRun && len(sizes) < len(Ladder) { // fail closed: a size left alone could run another number
		return fmt.Errorf("could not read this machine's bindings (%s), so Potluck cannot check that the sizes this run leaves alone run the same Linux slots: try again, or share every size (--sizes %s)", o.EarlierUnread, ladderNames())
	}
	for _, t := range shareBindings(o) {
		if bound[t.Name] {
			continue
		}
		if v := slotsOf(t); v != n {
			return fmt.Errorf("--linux-slots %d%s: binding %s is not in this run and runs %d Linux boxes at once, and every size of one machine must carry the same number (the driver counts every live box against each): share it in this run too (--sizes), or give it the same number",
				n, keptFrom(from), t.Name, v)
		}
	}
	return nil
}

// slotsOf is the Linux boxes a binding lets run at once.
func slotsOf(t computeapi.RunTarget) int {
	if k, err := strconv.Atoi(t.Config["linux_slots"]); err == nil {
		return k
	}
	return Slots
}

// shareBindings is this share's earlier box bindings on this machine
// (<prefix>-<size>, in the scope it binds in), by name.
func shareBindings(o ShareOptions) []computeapi.RunTarget {
	var out []computeapi.RunTarget
	for _, t := range o.Earlier {
		size, ok := strings.CutPrefix(t.Name, o.NamePrefix+"-")
		if !ok || t.Kind != "box" || t.Host != "" {
			continue
		}
		if _, err := SizeByName(size); err == nil {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b computeapi.RunTarget) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// oneAtATime reports whether a binding carries OneAtATimeConfig.
func oneAtATime(t computeapi.RunTarget) bool {
	for k, v := range OneAtATimeConfig {
		if t.Config[k] != v {
			return false
		}
	}
	return true
}

// macOSImages: every image is a pinned macOS guest on tart, so the boxes
// use macos_slots. An image Potluck has no pin for counts as Linux, whose
// rule (fit N times) is the stricter.
func macOSImages(layer string, images []string) bool {
	if layer != "tart" || len(images) == 0 {
		return false
	}
	for _, im := range images {
		if p, ok := PinForImage(im); !ok || p.OS != "darwin" {
			return false
		}
	}
	return true
}

// slotsFullOS is the guest OS whose slots were all taken when conformance
// failed: its first failure is the driver's refusal (a later one could be a
// driver that leaks slots, which must stay a plain failure). "" when not.
func slotsFullOS(stdout string) string {
	first := conformance.FirstFailure(stdout)
	for _, os := range []string{"linux", "darwin"} {
		if strings.Contains(first, vmhost.NoFreeSlot(os)) {
			return os
		}
	}
	return ""
}

// ladderNames is every size's name, for --sizes.
func ladderNames() string {
	var n []string
	for _, s := range Ladder {
		n = append(n, s.Name)
	}
	return strings.Join(n, ",")
}

// timesWord is "twice", or "N times".
func timesWord(n int) string {
	if n == 2 {
		return "twice"
	}
	return fmt.Sprintf("%d times", n)
}

// keptFrom is " (kept from <binding>)", or "" for the flag.
func keptFrom(from string) string {
	if from == "" {
		return ""
	}
	return " (kept from " + from + ")"
}

// slotsFullError is a size's error when conformance found every slot taken:
// boxes already running on this machine hold them, and conformance's own
// boxes need slots too. total is the slots conformance ran with.
func slotsFullError(os string, total int) string {
	if os == "darwin" {
		return fmt.Sprintf("every macOS slot on this Mac is in use (Apple's licence allows %d at once), and conformance needs free ones for its own boxes: try again when a running macOS VM ends", total)
	}
	return fmt.Sprintf("every Linux slot on this machine is in use by live boxes (%d at once), and conformance needs free ones for its own boxes (two for its peers check): try again when one ends, or raise --linux-slots", total)
}
