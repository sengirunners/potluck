package events

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

type fx struct {
	bus          *Bus
	orchid, side string
	owner, guest *authz.Subject
}

func newFx(t *testing.T) *fx {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fx{orchid: ids.New(), side: ids.New()}
	for _, p := range []string{f.orchid, f.side} {
		if _, err := st.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if f.bus, err = New(ctx, st, "mini"); err != nil {
		t.Fatal(err)
	}
	f.owner = &authz.Subject{Scope: authz.All()}
	f.guest = &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Guest, Object: authz.AreaObject(f.orchid, "sprout")}}, nil)}
	return f
}

func (f *fx) seed(t *testing.T) {
	ctx := context.Background()
	for _, in := range []Input{
		{Type: "dev.zm.daemon.started", Subject: "mini"},
		{Type: "dev.zm.work.claimed", Subject: "bd:sprout-1", Project: f.orchid, Area: "sprout",
			Data: map[string]any{"key": "bd:sprout-1", "holder": "mini/abcd1234", "note": "held by mini/abcd1234"}, Mentions: []string{"mini/abcd1234"}},
		{Type: "dev.zm.mail.sent", Subject: "01B", Project: f.orchid, Area: "billing", Data: "billing secret"},
		{Type: "dev.zm.mail.sent", Subject: "01S", Project: f.side, Data: "side secret"},
		{Type: "dev.zm.mail.sent", Subject: "01P", Project: f.orchid, Data: "project-level"},
		{Type: "dev.zm.mail.acked", Subject: "01S2", Project: f.orchid, Area: "sprout", Data: map[string]string{"acked_by": "human:judy"}, Mentions: []string{"human:judy"}},
	} {
		if _, err := f.bus.Publish(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSincePerSubscriberFilterAndRedaction(t *testing.T) {
	f := newFx(t)
	f.seed(t)
	ctx := context.Background()
	all, err := f.bus.Since(ctx, f.owner, "", Filter{}, 0)
	if err != nil || len(all) != 6 {
		t.Fatalf("owner sees %d %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatal("not ordered by id")
		}
	}
	got, err := f.bus.Since(ctx, f.guest, "", Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("guest sees %d: %+v", len(got), got)
	}
	blob := ""
	for _, e := range got {
		blob += string(e.Data) + e.Subject + e.Project + e.Area
	}
	for _, leak := range []string{"secret", "project-level", f.side, "mini/abcd1234", "daemon"} {
		if strings.Contains(blob, leak) {
			t.Errorf("guest stream leaks %q: %s", leak, blob)
		}
	}
	if !strings.Contains(blob, `held by someone`) || !strings.Contains(blob, "human:judy") {
		t.Errorf("redaction wrong: %s", blob)
	}
	// Resume after the first guest event: only the second.
	more, _ := f.bus.Since(ctx, f.guest, got[0].ID, Filter{}, 0)
	if len(more) != 1 || more[0].ID != got[1].ID {
		t.Fatalf("resume: %+v", more)
	}
	if _, err := f.bus.Since(ctx, f.guest, "42", Filter{}, 0); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatal("numeric cursor accepted")
	}
	typed, _ := f.bus.Since(ctx, f.owner, "", Filter{Types: []string{"dev.zm.mail."}, Project: f.orchid}, 0)
	if len(typed) != 3 {
		t.Fatalf("type+project filter: %d", len(typed))
	}
}

func TestLiveSubscribeFiltersAndRedacts(t *testing.T) {
	f := newFx(t)
	ch, cancel := f.bus.Subscribe(f.guest, Filter{}, 16)
	defer cancel()
	f.seed(t)
	var got []api.Event
	timeout := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case e := <-ch:
			got = append(got, e)
		case <-timeout:
			t.Fatalf("got %d live events", len(got))
		}
	}
	select {
	case e := <-ch:
		t.Fatalf("extra event leaked: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
	if strings.Contains(string(got[0].Data), "abcd1234") {
		t.Fatal("live event not redacted")
	}
}

func TestSSEReplayThenLive(t *testing.T) {
	f := newFx(t)
	f.seed(t)
	h := f.bus.Handler(HandlerOptions{
		Subject: func(*http.Request) *authz.Subject { return f.guest },
		Project: func(_ *http.Request, ref string) (string, error) { return "", api.NotFound("project", ref) },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?since=0&live=0")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	_ = ReadSSE(resp.Body, func(e api.Event) error { ids = append(ids, e.ID); return nil })
	resp.Body.Close()
	if len(ids) != 2 {
		t.Fatalf("replay %d", len(ids))
	}

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Last-Event-ID", ids[0])
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.bus.Publish(context.Background(), Input{Type: "dev.zm.side.x", Project: f.side})
		f.bus.Publish(context.Background(), Input{Type: "dev.zm.sprout.live", Project: f.orchid, Area: "sprout"})
	}()
	var seen []string
	err = ReadSSE(resp.Body, func(e api.Event) error {
		seen = append(seen, e.Type)
		if e.Type == "dev.zm.sprout.live" {
			return ErrStop
		}
		return nil
	})
	if err != nil || len(seen) != 2 || seen[0] != "dev.zm.mail.acked" {
		t.Fatalf("resume+live: %v %v", seen, err)
	}

	resp2, _ := http.Get(srv.URL + "?project=side")
	if resp2.StatusCode != 404 {
		t.Fatalf("hidden project filter: %d", resp2.StatusCode)
	}
	resp2.Body.Close()
}

// OwnerOnly keys reach only the machine's owner in person (scope all, via
// local), live and replayed; every other reader, the daemon's own followers
// (webhooks) included, gets the event without them. A scrubbed copy (an
// export) has none.
func TestOwnerOnlyKeys(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	member := &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Member, Object: authz.ProjectObject(f.orchid)}}, nil)}
	live := map[string]<-chan api.Event{}
	person := &authz.Subject{Scope: authz.All(), Via: authz.ViaLocal}
	subs := map[string]*authz.Subject{"owner": person, "member": member, "guest": f.guest, "system": authz.System("mini")}
	for name, sub := range subs {
		ch, cancel := f.bus.Subscribe(sub, Filter{}, 16)
		t.Cleanup(cancel)
		live[name] = ch
	}
	data := map[string]any{"address": "mini/abcd1234", "term_title": "ssh prod-db-01", "state": "running"}
	if _, err := f.bus.Publish(ctx, Input{Type: "dev.zm.session.updated", Subject: "mini/abcd1234", Project: f.orchid,
		Area: "sprout", Data: data, OwnerOnly: []string{"term_title"}}); err != nil {
		t.Fatal(err)
	}
	check := func(how, name string, e api.Event) {
		t.Helper()
		has := strings.Contains(string(e.Data), "prod-db-01")
		if has != (name == "owner") || !strings.Contains(string(e.Data), `"state":"running"`) {
			t.Errorf("%s %s: %s", how, name, e.Data)
		}
	}
	for name, ch := range live {
		select {
		case e := <-ch:
			check("live", name, e)
		case <-time.After(2 * time.Second):
			t.Fatalf("live %s: no event", name)
		}
	}
	for name, sub := range subs {
		evs, err := f.bus.Since(ctx, sub, "", Filter{Types: []string{"dev.zm.session."}}, 0)
		if err != nil || len(evs) != 1 {
			t.Fatalf("replay %s: %v %v", name, evs, err)
		}
		check("replay", name, evs[0])
	}
}

func TestScrubOwnerOnly(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := ids.New()
	db, err := st.CreateProject(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := New(ctx, st, "mini")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []Input{
		{Type: "dev.zm.session.updated", Subject: "mini/a", Project: p, Data: map[string]any{"state": "running", "ports": []int{3000}}, OwnerOnly: []string{"ports"}},
		{Type: "dev.zm.session.updated", Subject: "mini/b", Project: p, Data: map[string]any{"state": "idle", "ports": []int{8080}}},
		{Type: "dev.zm.session.updated", Subject: "mini/c", Project: p, Data: map[string]any{"state": "busy", "last_prompt": "secret plan"}, MembersOnly: []string{"last_prompt"}},
	} {
		if _, err := bus.Publish(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if err := ScrubOwnerOnly(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := ScrubMembersOnly(ctx, db); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT subject, json FROM events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var s, js string
		_ = rows.Scan(&s, &js)
		got[s] = js
	}
	if strings.Contains(got["mini/a"], "3000") || !strings.Contains(got["mini/a"], "running") || !strings.Contains(got["mini/b"], "8080") {
		t.Fatalf("scrubbed %v", got)
	}
	if strings.Contains(got["mini/c"], "secret plan") || !strings.Contains(got["mini/c"], "busy") {
		t.Fatalf("members-only left: %v", got["mini/c"])
	}
}

// A Live event reaches live subscribers and is never stored: Since does
// not replay it, and its id is a resume cursor that skips no stored event.
func TestLiveNotStored(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	ch, cancel := f.bus.Subscribe(f.owner, Filter{Types: []string{"dev.zm.test."}}, 16)
	defer cancel()
	first, err := f.bus.Publish(ctx, Input{Type: "dev.zm.test.stored", Subject: "a", Project: f.orchid, Data: 1})
	if err != nil {
		t.Fatal(err)
	}
	live, err := f.bus.Publish(ctx, Input{Type: "dev.zm.test.live", Subject: "a", Project: f.orchid, Data: 2, Live: true})
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.bus.Publish(ctx, Input{Type: "dev.zm.test.stored", Subject: "a", Project: f.orchid, Data: 3})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i := 0; i < 3; i++ {
		select {
		case e := <-ch:
			got = append(got, e.ID)
		case <-time.After(2 * time.Second):
			t.Fatalf("live: %v", got)
		}
	}
	if got[0] != first.ID || got[1] != live.ID || got[2] != after.ID || !(first.ID < live.ID && live.ID < after.ID) {
		t.Fatalf("live order %v", got)
	}
	all, err := f.bus.Since(ctx, f.owner, "", Filter{Types: []string{"dev.zm.test."}}, 0)
	if err != nil || len(all) != 2 || all[0].ID != first.ID || all[1].ID != after.ID {
		t.Fatalf("stored: %v %v", all, err)
	}
	resumed, err := f.bus.Since(ctx, f.owner, live.ID, Filter{Types: []string{"dev.zm.test."}}, 0)
	if err != nil || len(resumed) != 1 || resumed[0].ID != after.ID {
		t.Fatalf("resume from the live id: %v %v", resumed, err)
	}
}
