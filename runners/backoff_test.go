package runners_test

import (
	"context"
	"testing"
	"time"
)

// One error after many waits for a box is retried within Retry: the backoff
// counts errors in a row, not the waits before them. Errors in a row double
// it and drop the delivery at the sixth.
func TestErrorAfterWaitsBacksOffByFailsNotTries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	retry := 30 * time.Second
	f.svc.Retry, f.svc.WaitMax = retry, 5*time.Minute
	slack := 5 * time.Second // the fixture clock is the wall clock

	tries, fails := 0, 0
	for range 16 {
		n, gap, dropped, err := f.svc.SettleTry(ctx, "d1", tries, fails, true)
		if err != nil {
			t.Fatal(err)
		}
		if dropped || n != 0 || gap > 5*time.Minute+slack {
			t.Fatalf("wait %d: fails %d gap %s dropped %v, want fails 0 within WaitMax", tries+1, n, gap, dropped)
		}
		tries, fails = tries+1, n
	}
	for i := 1; i <= 6; i++ {
		n, gap, dropped, err := f.svc.SettleTry(ctx, "d1", tries, fails, false)
		if err != nil {
			t.Fatal(err)
		}
		if i == 6 {
			if !dropped || n != 6 {
				t.Fatalf("error %d: fails %d dropped %v, want dropped at 6", i, n, dropped)
			}
			break
		}
		want := retry << (i - 1)
		if dropped || n != i || gap > want+slack || gap < want-slack {
			t.Fatalf("error %d after %d tries: fails %d gap %s dropped %v, want fails %d gap %s", i, tries, n, gap, dropped, i, want)
		}
		tries, fails = tries+1, n
	}
}
