package events

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// zm-p7fk: events a reader may not see fill a raw page; replay must keep
// going past them instead of treating a short page as the end of the log.
func TestReplaySkipsPagesOfHiddenEvents(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	pub := func(in Input) {
		if _, err := f.bus.Publish(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	// More hidden events first than one raw scan reads (sinceScan), then 60
	// the guest reads, with hidden ones between them as well.
	for i := 0; i < sinceScan+20; i++ {
		pub(Input{Type: "dev.zm.mail.sent", Subject: fmt.Sprintf("owners-%d", i), Project: f.orchid, Area: "sprout",
			Audience: &Audience{Need: authz.Admin}})
	}
	for i := 0; i < 60; i++ {
		pub(Input{Type: "dev.zm.mail.sent", Subject: fmt.Sprintf("g1-%d", i), Project: f.orchid, Area: "sprout"})
		if i%3 == 0 {
			pub(Input{Type: "dev.zm.mail.sent", Subject: fmt.Sprintf("billing-%d", i), Project: f.orchid, Area: "billing"})
		}
	}

	// Since answers a full page while more visible events exist.
	page, err := f.bus.Since(ctx, f.guest, "", Filter{}, 50)
	if err != nil || len(page) != 50 {
		t.Fatalf("first page: %d %v", len(page), err)
	}
	rest, err := f.bus.Since(ctx, f.guest, page[len(page)-1].ID, Filter{}, 50)
	if err != nil || len(rest) != 10 {
		t.Fatalf("second page: %d %v", len(rest), err)
	}

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
	defer resp.Body.Close()
	n := 0
	_ = ReadSSE(resp.Body, func(e api.Event) error {
		if want := fmt.Sprintf("g1-%d", n); e.Subject != want {
			t.Fatalf("event %d is %q, want %q", n, e.Subject, want)
		}
		n++
		return nil
	})
	if n != 60 {
		t.Fatalf("SSE replay delivered %d of 60 visible events", n)
	}
}

// The same across logs: one project's log is all hidden rows, while the
// events the reader may see sit in another project's log.
func TestSinceAcrossLogsSkipsHiddenPages(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	g := &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{
		{Relation: authz.Guest, Object: authz.AreaObject(f.orchid, "sprout")},
		{Relation: authz.Guest, Object: authz.AreaObject(f.side, "leaf")},
	}, nil)}
	want := 0
	for i := 0; i < 2*sinceScan+30; i++ {
		in := Input{Type: "dev.zm.mail.sent", Subject: fmt.Sprintf("h-%d", i), Project: f.orchid, Area: "sprout", Audience: &Audience{Need: authz.Admin}}
		if i%40 == 0 {
			in = Input{Type: "dev.zm.mail.sent", Subject: fmt.Sprintf("v-%d", want), Project: f.side, Area: "leaf"}
			want++
		}
		if _, err := f.bus.Publish(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	var got []api.Event
	cursor := ""
	for {
		page, err := f.bus.Since(ctx, g, cursor, Filter{Types: []string{"dev.zm.mail."}}, 7)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, page...)
		if len(page) < 7 {
			break
		}
		cursor = page[len(page)-1].ID
	}
	if len(got) != want {
		t.Fatalf("got %d of %d visible events", len(got), want)
	}
	for i, e := range got {
		if e.Subject != fmt.Sprintf("v-%d", i) {
			t.Fatalf("event %d is %q", i, e.Subject)
		}
	}
}
