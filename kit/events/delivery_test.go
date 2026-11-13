package events

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
)

func TestDurableEventDeliveryReplayAndConflict(t *testing.T) {
	for _, team := range []bool{false, true} {
		t.Run(map[bool]string{false: "solo", true: "team"}[team], func(t *testing.T) {
			f := newFx(t)
			ctx := context.Background()
			if team {
				if err := f.bus.st.SetTeam(ctx, f.orchid, true); err != nil {
					t.Fatal(err)
				}
			}
			in := Input{DeliveryID: ids.New(), Type: evDecided, Project: f.orchid, Subject: "decision", Data: map[string]string{"text": "sensitive answer"}}
			ch, cancel := f.bus.Subscribe(f.owner, Filter{}, 10)
			defer cancel()
			receive := func(want api.Event) {
				t.Helper()
				select {
				case got, ok := <-ch:
					if !ok || got.ID != want.ID || got.Type != want.Type || got.Project != want.Project {
						t.Fatalf("live delivery: got %+v, open=%t; want %+v", got, ok, want)
					}
				case <-time.After(3 * time.Second):
					t.Fatalf("live delivery did not reach subscriber: %s", want.ID)
				}
			}
			first, err := f.bus.Publish(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			again, err := f.bus.Publish(ctx, in)
			if err != nil || again.ID != first.ID {
				t.Fatalf("replay %+v %v", again, err)
			}
			// Subscribe forwards through an internal queue asynchronously.
			// Await the actual first delivery instead of sampling queue length.
			receive(first)
			in.Data = map[string]string{"text": "changed answer"}
			if _, err := f.bus.Publish(ctx, in); !api.IsCode(err, api.CodeConflict) {
				t.Fatalf("payload conflict: %v", err)
			}
			in.Data = map[string]string{"text": "sensitive answer"}
			db := f.bus.st.Writer(f.orchid)
			if _, err := db.ExecContext(ctx, `DELETE FROM events`); err != nil {
				t.Fatal(err)
			}
			again, err = f.bus.Publish(ctx, in)
			if err != nil || again.ID != first.ID {
				t.Fatalf("pruned replay: %+v %v", again, err)
			}
			var n int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("recreated pruned event: %d %v", n, err)
			}
			// Publish enqueues live deliveries in order before returning. A later
			// distinct event therefore proves all earlier replay/conflict paths
			// are drained: any duplicate must arrive before this barrier and fail
			// its exact identity check. No quiet-time wait can establish that.
			barrierInput := in
			barrierInput.DeliveryID, barrierInput.Subject = ids.New(), "live barrier"
			barrier, err := f.bus.Publish(ctx, barrierInput)
			if err != nil {
				t.Fatal(err)
			}
			receive(barrier)
		})
	}
}
