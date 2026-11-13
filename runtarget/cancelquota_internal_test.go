package runtarget

import (
	"testing"
	"time"
)

func TestCancelQuotaTakeAll(t *testing.T) {
	var q cancelQuota
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	member := func(m string) quotaCap { return quotaCap{"member:" + m, 2} }
	if who, wait := (&cancelQuota{}).takeAll(now, quotaCap{"none", 0}); who != "none" || wait != cancelKept {
		t.Fatalf("a cap of nothing: %q %v", who, wait)
	}
	project := quotaCap{"project:p", 3}

	for _, c := range []string{"a", "a", "b"} {
		if who, _ := q.takeAll(now, member(c), project); who != "" {
			t.Fatalf("%s refused by %s while under both caps", c, who)
		}
	}
	// The project is at 3: a member with room is refused by the project, and
	// nothing is counted against the member.
	if who, _ := q.takeAll(now, member("c"), project); who != "project:p" {
		t.Fatalf("a new member past the project's cap: refused by %q, want the project", who)
	}
	if got := len(q.m["member:c"]); got != 0 {
		t.Errorf("a refused cancel was counted against its member: %d", got)
	}
	// A member at their own cap is refused by the member, not counted for the project.
	q = cancelQuota{}
	q.takeAll(now, member("a"), project)
	q.takeAll(now, member("a"), project)
	who, wait := q.takeAll(now.Add(time.Hour), member("a"), project)
	if who != "member:a" || wait != 23*time.Hour {
		t.Errorf("member at its cap an hour on: %q wait %v, want member:a, 23h (when its oldest leaves)", who, wait)
	}
	if got := len(q.m["project:p"]); got != 2 {
		t.Errorf("project count after a member's refusal: %d, want 2", got)
	}
	// A day later the count has room again.
	if who, _ := q.takeAll(now.Add(cancelKept), member("a"), project); who != "" {
		t.Errorf("after the cancels aged out: refused by %q", who)
	}
}
