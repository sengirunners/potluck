package teamlink_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// fakePart is a beat part that sends a counter (or, when big is set, a
// value over the beat's body limit), and records what the authority's side
// was sent and what the member's side was answered.
type fakePart struct {
	mu      sync.Mutex
	n       int
	big     atomic.Bool
	served  []string // at the authority: "<machine> <project> <value>"
	answers []string // at the member
}

func (f *fakePart) part() teamlink.BeatPart {
	return teamlink.BeatPart{
		Request: func(_ context.Context, b teamlink.Beat) (json.RawMessage, func(context.Context, json.RawMessage), error) {
			if f.big.Load() {
				return json.RawMessage(`"` + string(bytes.Repeat([]byte("x"), teamlink.MaxBeatBody)) + `"`), nil, nil
			}
			f.mu.Lock()
			f.n++
			v, _ := json.Marshal(map[string]any{"n": f.n, "slug": b.Snap.Project.Slug})
			f.mu.Unlock()
			return v, func(_ context.Context, answer json.RawMessage) {
				f.mu.Lock()
				f.answers = append(f.answers, string(answer))
				f.mu.Unlock()
			}, nil
		},
		Serve: func(ctx context.Context, p api.Project, v json.RawMessage) (json.RawMessage, error) {
			machine := core.SubjectOf(ctx).Machine
			f.mu.Lock()
			f.served = append(f.served, machine+" "+p.ID+" "+string(v))
			f.mu.Unlock()
			return json.Marshal(map[string]any{"to": machine, "got": v})
		},
	}
}

func (f *fakePart) counts() (served, answers int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.served), len(f.answers)
}

// Two KIT-only daemons (no internal/team): a joined member's heartbeat
// carries a registered part's value to the authority and its answer back,
// carries a machine op from the authority's queue and its report back, and
// reports the machine's numbers, which the authority keeps in memory and
// lists on GET /v0/machines; an oversized beat is not sent, and refused
// when posted; the loop beats by itself on a kick.
func TestBeatFakePart(t *testing.T) {
	teamlink.BeatsOff.Store(true) // beats by hand until the last step
	t.Cleanup(func() { teamlink.BeatsOff.Store(false) })
	ctx := context.Background()
	tn := tailnettest.New()
	var ap, mp fakePart
	var runs atomic.Int64
	setup := func(fp *fakePart) func(*teamlink.Service) {
		return func(link *teamlink.Service) {
			link.RegisterBeatPart("fake", fp.part())
			link.RegisterOp("beat_echo", teamlink.OpHandler{Run: func(_ context.Context, _ string, op api.MachineOp) (json.RawMessage, error) {
				runs.Add(1)
				return op.Payload, nil
			}})
		}
	}
	a, alink := linkDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, setup(&ap))
	m, mlink := linkDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"}, setup(&mp))

	p, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ProjectTeam(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	inv, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana", Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil {
		t.Fatal(err)
	}
	x := mlink.ByName("orchid")
	if x == nil {
		t.Fatal("no remote after join")
	}

	// The part's request reaches the authority's side, its answer the
	// member's.
	mlink.Heartbeat(ctx)
	ap.mu.Lock()
	served := append([]string(nil), ap.served...)
	ap.mu.Unlock()
	if len(served) != 1 || served[0] != `laptop `+p.ID+` {"n":1,"slug":"orchid"}` {
		t.Fatalf("authority's part was served %q", served)
	}
	mp.mu.Lock()
	answers := append([]string(nil), mp.answers...)
	mp.mu.Unlock()
	if len(answers) != 1 || answers[0] != `{"got":{"n":1,"slug":"orchid"},"to":"laptop"}` {
		t.Fatalf("member's part was answered %q", answers)
	}
	if n, _ := mp.counts(); n != 0 {
		t.Fatalf("the member served %d beats", n)
	}

	// A machine op queued at the authority goes out with a beat's answer,
	// runs on the member once, and its report rides the next beat.
	id := ids.New()
	if err := alink.QueueOp(ctx, teamlink.Op{MachineOp: api.MachineOp{ID: id, Kind: "beat_echo", Payload: json.RawMessage(`{"k":7}`)},
		Project: p.ID, Machine: "laptop", FromMachine: "mini"}); err != nil {
		t.Fatal(err)
	}
	mlink.Heartbeat(ctx)
	waitFor(t, "the op to run on the member", func() bool { return runs.Load() == 1 })
	waitFor(t, "the op's report", func() bool {
		mlink.Heartbeat(ctx)
		o, err := alink.LoadOp(ctx, id)
		return err == nil && o.State == teamlink.OpDone
	})
	if o, _ := alink.LoadOp(ctx, id); o.Result != `{"k":7}` || o.Error != "" {
		t.Fatalf("op through the beat: %+v", o)
	}
	if runs.Load() != 1 {
		t.Fatalf("ran %d times", runs.Load())
	}

	// The member's numbers, kept in memory at the authority (no module keeps
	// them), are listed with the authority's own.
	var l api.MachineList
	if err := a.Do(ctx, http.MethodGet, "/v0/machines", nil, &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Machines) != 2 || l.Machines[0].Machine != "mini" || l.Machines[1].Machine != "laptop" ||
		len(l.Machines[1].Projects) != 1 || l.Machines[1].Projects[0] != p.ID || l.Machines[1].Seen == nil {
		t.Fatalf("machines: %+v", l.Machines)
	}

	// A beat over the body limit is not sent; posted anyway, it is refused.
	before, _ := ap.counts()
	mp.big.Store(true)
	mlink.Heartbeat(ctx)
	mp.big.Store(false)
	if n, _ := ap.counts(); n != before {
		t.Fatalf("an oversized beat was served")
	}
	big := `{"project":"` + p.ID + `","parts":{"fake":"` + string(bytes.Repeat([]byte("x"), teamlink.MaxBeatBody)) + `"}}`
	if err := mlink.CallRemote(ctx, x, http.MethodPost, "/v0/team/presence", json.RawMessage(big), nil); !api.IsCode(err, api.CodeTooLarge) {
		t.Fatalf("oversized beat posted: %v", err)
	}
	ops := make([]api.MachineOpReport, 201)
	for i := range ops {
		ops[i] = api.MachineOpReport{ID: ids.New()}
	}
	if err := mlink.CallRemote(ctx, x, http.MethodPost, "/v0/team/presence", api.PresenceRequest{Project: p.ID, Ops: ops}, nil); !api.IsCode(err, api.CodeTooLarge) {
		t.Fatalf("201 op reports posted: %v", err)
	}
	if n, _ := ap.counts(); n != before {
		t.Fatalf("a refused beat was served")
	}

	// teamlink runs the loop: a kick beats soon.
	teamlink.BeatsOff.Store(false)
	mlink.KickBeat()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n, _ := ap.counts(); n > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no beat after a kick")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Registering a beat part twice panics.
func TestRegisterBeatPartTwicePanics(t *testing.T) {
	s := teamlink.New(&core.Deps{})
	s.RegisterBeatPart("p", teamlink.BeatPart{})
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	s.RegisterBeatPart("p", teamlink.BeatPart{})
}
