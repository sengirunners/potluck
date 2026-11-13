// Package placement is the one rule for which box binding of a pool takes a
// new box: sandboxes (computeclient.PlaceBox) and GitHub runner jobs
// (runners) both judge and rank bindings here, so the pool's order and its
// reasons are the same everywhere.
//
// The order (SPEC 20, Placement): this machine's own bindings (the team's),
// then trusted hosts, then bindings whose driver charges per use (cloud
// overflow); a public host is never picked unless the caller names it. Within
// a tier the smallest size that fits wins (fewest CPUs, then least memory; an
// unstated figure last), then the least busy, then the name.
package placement

import (
	"cmp"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Tier is where a binding stands in the pool's order; lower is preferred.
type Tier int

const (
	// Team: a binding this machine serves itself.
	Team Tier = iota
	// Trusted: a binding a trusted host serves.
	Trusted
	// Paid: a binding whose driver charges per use (a cloud driver), ranked
	// after every free one.
	Paid
	// Public: a public host's binding (a lender's or a stranger's machine):
	// placement never picks one.
	Public
)

func (t Tier) String() string {
	switch t {
	case Team:
		return "team"
	case Trusted:
		return "trusted"
	case Paid:
		return "paid"
	}
	return "public"
}

// TierOf is v's tier. Any class but trusted counts as public (fail closed).
func TierOf(v computeapi.RunTargetView) Tier {
	switch {
	case v.HostClass != api.MachineTrusted:
		return Public
	case v.Describe != nil && v.Describe.Cost.Amount > 0:
		return Paid
	case v.HostBinding != "":
		return Trusted
	}
	return Team
}

// Want is what a new box asks of every binding, whoever places it.
type Want struct {
	// Size is the size name a binding must state ("" any).
	Size string
	// OS is the OS a binding must state ("" any; macos takes darwin).
	OS string
	// TTLS is how long the box must be allowed to live (0: no need), and
	// Lease what needs that long in the reason ("": the box).
	TTLS  int
	Lease string
	// Public is the reason a public host is passed over ("": a public host
	// (placement never picks one)).
	Public string
}

// Refusal is why v cannot take a box for w, or "" when it can. can is the
// caller's own needs (capabilities, images), asked once describe answered
// and before availability, so a binding that could never serve is not
// reported as merely busy. Text from drivers is quoted (untrusted).
func Refusal(v computeapi.RunTargetView, w Want, can func(*computeapi.RunTargetDescribe) string) string {
	d := v.Describe
	switch {
	case v.Kind != computeapi.RunTargetKindBox:
		return "not a box run target"
	case TierOf(v) == Public:
		return cmp.Or(w.Public, "a public host (placement never picks one)")
	case v.Error != "":
		return "describe failed: " + Quote(v.Error)
	case d == nil:
		return "no describe"
	}
	if can != nil {
		if why := can(d); why != "" {
			return why
		}
	}
	switch c := LifeCap(v.TTLMaxS, d.TTLMaxS); {
	case !d.Available:
		return "busy or away: " + Quote(cmp.Or(d.Why, "no reason given"))
	case d.Size == nil:
		return "states no size"
	case w.Size != "" && d.Size.Name != w.Size:
		return fmt.Sprintf("size %s, want %s", Quote(d.Size.Name), w.Size)
	case !OSFits(w.OS, d.Size.OS):
		return fmt.Sprintf("%s, want %s", Quote(d.Size.OS), w.OS)
	case c > 0 && w.TTLS > 0 && c < w.TTLS:
		return fmt.Sprintf("boxes live at most %ds, %s needs %ds", c, cmp.Or(w.Lease, "the box"), w.TTLS)
	}
	if b := v.Budget; b != nil && v.BudgetSpent != nil {
		if why := b.Refusal(*v.BudgetSpent); why != "" {
			return why
		}
	}
	return ""
}

// Fit is one binding that can take the box, with what ranks it.
type Fit struct {
	Name string
	Tier Tier
	// Prefer is the caller's own order within a tier (lower first).
	Prefer int
	// CPU and MemoryMB are the size's figures (an unstated one ranks last).
	CPU, MemoryMB int
	// Load is how many boxes it serves now (fewer first).
	Load int
}

// FitOf is v's Fit; v has passed Refusal.
func FitOf(v computeapi.RunTargetView) Fit {
	unknown := func(n int) int {
		if n <= 0 {
			return math.MaxInt
		}
		return n
	}
	f := Fit{Name: v.Name, Tier: TierOf(v), CPU: math.MaxInt, MemoryMB: math.MaxInt}
	if d := v.Describe; d != nil && d.Size != nil {
		f.CPU, f.MemoryMB = unknown(d.Size.CPU), unknown(d.Size.MemoryMB)
	}
	return f
}

// Rank orders fits best first: tier, the caller's preference, fewest CPUs,
// least memory, least load, name.
func Rank(fits []Fit) []string {
	fits = append([]Fit(nil), fits...)
	sort.Slice(fits, func(i, j int) bool {
		a, b := fits[i], fits[j]
		return cmp.Or(cmp.Compare(a.Tier, b.Tier), cmp.Compare(a.Prefer, b.Prefer), cmp.Compare(a.CPU, b.CPU),
			cmp.Compare(a.MemoryMB, b.MemoryMB), cmp.Compare(a.Load, b.Load), cmp.Compare(a.Name, b.Name)) < 0
	})
	names := make([]string, len(fits))
	for i, f := range fits {
		names[i] = f.Name
	}
	return names
}

// LifeCap is the stricter of a binding's and its driver's ttl_max_s (0: none).
func LifeCap(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	}
	return min(a, b)
}

// OSFits: want "" takes any; GitHub's macos is a darwin box (Tart says
// darwin); otherwise the names match, case aside.
func OSFits(want, got string) bool {
	switch {
	case want == "":
		return true
	case strings.EqualFold(want, "macos"):
		return strings.EqualFold(got, "macos") || strings.EqualFold(got, "darwin")
	}
	return strings.EqualFold(want, got)
}

// Quote is a driver's words in a reason: one line, at most 120 characters,
// control characters escaped by the quoting.
func Quote(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "..."
	}
	return strconv.Quote(s)
}
