package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// A subscriber that falls behind is cut off (channel closes) instead of
// silently missing events, and Since(last) fills the gap exactly.
func TestSubscribeOverflowCutsOff(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	ch, cancel := f.bus.Subscribe(f.owner, Filter{}, 16)
	defer cancel()
	const n = 100
	for i := 0; i < n; i++ {
		if _, err := f.bus.Publish(ctx, Input{Type: "dev.zm.test", Subject: fmt.Sprint(i), Project: f.orchid}); err != nil {
			t.Fatal(err)
		}
	}
	var got []api.Event
	timeout := time.After(2 * time.Second)
drain:
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				break drain
			}
			got = append(got, e)
		case <-timeout:
			t.Fatalf("slow subscriber was not cut off (%d events delivered)", len(got))
		}
	}
	if len(got) >= n {
		t.Fatalf("no overflow with %d events", len(got))
	}
	last := ""
	if len(got) > 0 {
		last = got[len(got)-1].ID
	}
	rest, err := f.bus.Since(ctx, f.owner, last, Filter{Types: []string{"dev.zm.test"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	all := append(got, rest...)
	if len(all) != n {
		t.Fatalf("live %d + replay %d != %d", len(got), len(rest), n)
	}
	for i, e := range all {
		if e.Subject != fmt.Sprint(i) {
			t.Fatalf("event %d is %q", i, e.Subject)
		}
	}
}

// pipeWriter is a ResponseWriter whose writes block until the test reads them.
type pipeWriter struct {
	h  http.Header
	pw *io.PipeWriter
}

func (p *pipeWriter) Header() http.Header         { return p.h }
func (p *pipeWriter) WriteHeader(int)             {}
func (p *pipeWriter) Write(b []byte) (int, error) { return p.pw.Write(b) }
func (p *pipeWriter) Flush()                      {}

// A live SSE reader that falls behind gets ": overflow <id>" and the stream
// ends; resuming from the last event it handled loses nothing.
func TestSSEOverflowEndsStream(t *testing.T) {
	oldBuf, oldMin := StreamBuffer, MinBuffer
	StreamBuffer, MinBuffer = 1, 1
	t.Cleanup(func() { StreamBuffer, MinBuffer = oldBuf, oldMin })

	f := newFx(t)
	ctx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	h := f.bus.Handler(HandlerOptions{
		Subject: func(*http.Request) *authz.Subject { return f.guest },
		Project: func(_ *http.Request, ref string) (string, error) { return "", api.NotFound("project", ref) },
	})
	pr, pw := io.Pipe()
	req := httptest.NewRequest("GET", "/v0/events", nil).WithContext(ctx)
	go func() {
		h.ServeHTTP(&pipeWriter{h: http.Header{}, pw: pw}, req)
		pw.Close()
	}()

	gate := make(chan struct{})
	first := make(chan struct{})
	var seen []api.Event
	done := make(chan error, 1)
	go func() {
		done <- ReadSSE(pr, func(e api.Event) error {
			seen = append(seen, e)
			if len(seen) == 1 {
				close(first)
				<-gate // the reader stalls: the handler blocks writing, the bus fills up
			}
			return nil
		})
	}()
	pub := func(i int, area string) {
		if _, err := f.bus.Publish(context.Background(), Input{Type: "dev.zm.test", Subject: fmt.Sprint(i), Project: f.orchid, Area: area}); err != nil {
			t.Error(err)
		}
	}
	waitSubscribed(t, f.bus)
	pub(0, "sprout")
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("first event never arrived")
	}
	const n = 60
	for i := 1; i < n; i++ {
		pub(i, "sprout")
		pub(-i, "billing") // hidden from the guest: never delivered, never counted
	}
	close(gate)
	var err error
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not end after overflow")
	}
	var lag *LagError
	if !errors.As(err, &lag) || !errors.Is(err, ErrLagged) {
		t.Fatalf("want a LagError, got %v after %d events", err, len(seen))
	}
	if len(seen) >= n {
		t.Fatalf("no overflow: %d events", len(seen))
	}
	if lag.Last != seen[len(seen)-1].ID {
		t.Fatalf("overflow cursor %q, last event %q", lag.Last, seen[len(seen)-1].ID)
	}
	rest, err := f.bus.Since(context.Background(), f.guest, lag.Last, Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	all := append(seen, rest...)
	if len(all) != n {
		t.Fatalf("live %d + replay %d != %d", len(seen), len(rest), n)
	}
	for i, e := range all {
		if e.Subject != fmt.Sprint(i) || e.Area != "sprout" {
			t.Fatalf("event %d is %q/%q", i, e.Subject, e.Area)
		}
	}
}

func waitSubscribed(t *testing.T, b *Bus) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		b.mu.Lock()
		n := len(b.subs)
		b.mu.Unlock()
		if n > 0 {
			return
		}
	}
	t.Fatal("handler never subscribed")
}

// A live-only stream names its cursor right after the banner (": cursor
// <id>"), before any event: a reader cut off before the first event resumes
// from it and misses nothing published since the stream began, while a
// replay stream (which has its own cursor) sends none.
func TestSSELiveCursorAfterBanner(t *testing.T) {
	f := newFx(t)
	h := f.bus.Handler(HandlerOptions{
		Subject: func(*http.Request) *authz.Subject { return f.guest },
		Project: func(_ *http.Request, ref string) (string, error) { return "", api.NotFound("project", ref) },
	})
	readHead := func(target string) (string, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		pr, pw := io.Pipe()
		req := httptest.NewRequest("GET", target, nil).WithContext(ctx)
		go func() {
			h.ServeHTTP(&pipeWriter{h: http.Header{}, pw: pw}, req)
			pw.Close()
		}()
		buf := make([]byte, 0, 256)
		tmp := make([]byte, 256)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && strings.Count(string(buf), "\n\n") < 2 {
			n, err := pr.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		return string(buf), func() { cancel(); go io.Copy(io.Discard, pr) }
	}

	head, stop := readHead("/v0/events")
	stop()
	parts := strings.Split(head, "\n\n")
	if len(parts) < 2 || !strings.HasPrefix(parts[1], CursorComment+" ") {
		t.Fatalf("live stream head %q: want the banner then %q <id>", head, CursorComment)
	}
	cursor := strings.TrimPrefix(parts[1], CursorComment+" ")
	if len(cursor) != 26 {
		t.Fatalf("cursor %q is not an event id", cursor)
	}
	// Published after the stream began: resuming from the cursor has it.
	if _, err := f.bus.Publish(context.Background(), Input{Type: "dev.zm.test", Subject: "after", Project: f.orchid, Area: "sprout"}); err != nil {
		t.Fatal(err)
	}
	rest, err := f.bus.Since(context.Background(), f.guest, cursor, Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].Subject != "after" {
		t.Fatalf("resume from the cursor gave %v", rest)
	}

	head, stop = readHead("/v0/events?since=" + cursor + "&live=0")
	stop()
	if strings.Contains(head, CursorComment) {
		t.Fatalf("a replay stream sent a cursor comment: %q", head)
	}
}

// An event published while a live-only stream is starting (after it
// subscribes, around the moment its cursor is minted) is delivered on the
// stream: the cursor is minted after the subscription, so the stream itself
// never has a silent gap (minting it first lost such an event).
func TestSSELiveCursorAfterSubscribe(t *testing.T) {
	f := newFx(t)
	testHookSubscribed = func() {
		if _, err := f.bus.Publish(context.Background(), Input{Type: "dev.zm.test", Subject: "between", Project: f.orchid}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookSubscribed = nil })
	h := f.bus.Handler(HandlerOptions{
		Subject: func(*http.Request) *authz.Subject { return f.owner },
		Project: func(_ *http.Request, ref string) (string, error) { return "", api.NotFound("project", ref) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	req := httptest.NewRequest("GET", "/v0/events?types=dev.zm.test", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(&pipeWriter{h: http.Header{}, pw: pw}, req)
		pw.Close()
	}()
	got := make(chan string, 1)
	go func() {
		_ = ReadSSE(pr, func(e api.Event) error { got <- e.Subject; return ErrStop })
		cancel()
		io.Copy(io.Discard, pr)
	}()
	select {
	case s := <-got:
		if s != "between" {
			t.Fatalf("first live event %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an event published while the stream started never arrived on it")
	}
	cancel()
	<-done
}
