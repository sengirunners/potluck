package events

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
)

// Heartbeat is how often an idle SSE stream sends a comment line.
var Heartbeat = 15 * time.Second

// StreamBuffer is each SSE subscriber's live buffer (tests lower it).
var StreamBuffer = 256

// overflowComment ends a stream whose reader fell behind; it is followed by
// the last event id written (possibly empty).
const overflowComment = ": overflow"

// CursorComment opens a live-only stream (no since, or since=now), right
// after the banner: ": cursor <id>", an event id minted before the
// subscription. Resuming with Last-Event-ID: <id> replays every visible
// event published since the stream began.
const CursorComment = ": cursor"

// HandlerOptions wires the SSE handler to the server (events cannot import core).
type HandlerOptions struct {
	// Subject returns the authenticated caller of r (never nil).
	Subject func(r *http.Request) *authz.Subject
	// Project resolves ?project= (slug or ULID) for the caller to a ULID, or
	// returns api.NotFound("project", ref).
	Project func(r *http.Request, ref string) (string, error)
	// Done, when closed, ends every live stream (daemon shutdown), so the
	// server need not wait for readers that never hang up.
	Done <-chan struct{}
	// Still, when set, runs at each heartbeat of a live stream on project ("" =
	// every project): an error (the caller's access to it changed or ended) ends
	// the stream with that error.
	Still func(r *http.Request, project string) error
	// Tunnel, when set, reports whether r came over a tunnel: its events
	// are delivered without their Audience.NotOverTunnel keys.
	Tunnel func(r *http.Request) bool
}

// Handler serves GET /v0/events as Server-Sent Events, filtered and redacted
// per subscriber.
//
// Query: types (comma list of type prefixes), subject, project, since (an
// event id; "0" = everything stored; absent or "now" = live only; the
// Last-Event-ID header wins), live=0 (return after the replay). A live-only
// stream starts with ": cursor <id>" (CursorComment) after the banner.
//
// A live reader that falls StreamBuffer events behind gets
// ": overflow <last id>" and the stream ends; it reconnects with
// Last-Event-ID (client.Events does this itself).
//
//	id: <event ULID>
//	event: <type>
//	data: <CloudEvents JSON>
func (b *Bus) Handler(o HandlerOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := o.Subject(r)
		q := r.URL.Query()
		f := Filter{Types: ParseTypes(q.Get("types")), Subject: q.Get("subject"), OverTunnel: o.Tunnel != nil && o.Tunnel(r)}
		if p := q.Get("project"); p != "" {
			id, err := o.Project(r, p)
			if err != nil {
				api.WriteError(w, err)
				return
			}
			f.Project = id
		}
		since := q.Get("since")
		if id := r.Header.Get("Last-Event-ID"); id != "" {
			since = id
		}
		replay := since != "" && since != "now"
		if since == "0" {
			since = ""
		}
		live := q.Get("live") != "0"
		fl, ok := w.(http.Flusher)
		if !ok {
			api.WriteError(w, api.Errorf(api.CodeInternal, "streaming unsupported"))
			return
		}
		if replay && since != "" && !ids.Valid(since) {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "bad event cursor %q (want an event id)", since))
			return
		}
		// Subscribe before replaying so nothing published in between is lost.
		ch, cancel := b.Subscribe(sub, f, StreamBuffer)
		defer cancel()
		if testHookSubscribed != nil {
			testHookSubscribed()
		}
		last := since
		// floor drops live events the replay already wrote; a live-only
		// stream has none, so it delivers everything it is subscribed to.
		floor := since
		if !replay {
			// Live only: the cursor is minted after the subscription, so
			// every event after it is both on this stream and replayable
			// from it (no silent gap); one published in between is
			// delivered live and is older than the cursor.
			last, floor = ids.New(), ""
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, ": "+api.WireProtocol+" events\n\n")
		if !replay {
			// A live-only stream names its starting cursor at once, so a
			// reader cut off before any event resumes with Last-Event-ID
			// from here instead of reloading everything.
			fmt.Fprintf(w, "%s %s\n\n", CursorComment, last)
		}
		fl.Flush()

		// A small first page puts the first event on the wire at once; the
		// rest of the replay goes in pages of 500.
		for page := 50; replay; page = 500 {
			batch, err := b.Since(r.Context(), sub, last, f, page)
			if err != nil {
				return
			}
			for _, e := range batch {
				if writeEvent(w, e) != nil {
					return
				}
				last, floor = e.ID, e.ID
			}
			fl.Flush()
			if len(batch) < page {
				break
			}
		}
		if !live {
			return
		}
		tick := time.NewTicker(Heartbeat)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-o.Done:
				fmt.Fprint(w, ": daemon stopping\n\n")
				fl.Flush()
				return
			case <-tick.C:
				// A live stream outlasts the check its request passed: access
				// that ended ends it, within one heartbeat.
				if o.Still != nil {
					if err := o.Still(r, f.Project); err != nil {
						WriteStreamError(w, err)
						fl.Flush()
						return
					}
				}
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				fl.Flush()
			case e, ok := <-ch:
				if !ok {
					// Cut off for falling behind: say so, end the stream, and
					// let the client resume from `last` (never skip silently).
					fmt.Fprintf(w, "%s %s\n\n", overflowComment, last)
					fl.Flush()
					return
				}
				if floor != "" && e.ID <= floor {
					continue
				}
				if writeEvent(w, e) != nil {
					return
				}
				last, floor = e.ID, e.ID
				fl.Flush()
			}
		}
	})
}

// testHookSubscribed runs right after a stream subscribes (tests only).
var testHookSubscribed func()

// WriteStreamError ends a stream with why it ended, as a comment: a reader that
// ignores comments sees the stream end; ReadSSE returns a StreamError.
func WriteStreamError(w io.Writer, err error) {
	fmt.Fprintf(w, "%s %s\n\n", streamErrorComment, strings.NewReplacer("\n", " ", "\r", " ").Replace(err.Error()))
}

const streamErrorComment = ": error"

// StreamError is why the far end ended a stream.
type StreamError struct{ Message string }

func (e *StreamError) Error() string { return e.Message }

func writeEvent(w io.Writer, e api.Event) error {
	js, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", e.ID, e.Type, js)
	return err
}

// ReadSSE parses an SSE stream written by Handler, calling fn for each event
// until the stream ends, fn returns an error, or the stream is malformed.
// ErrStop from fn ends reading with a nil error. A stream the server cut off
// for falling behind returns a *LagError (errors.Is ErrLagged).
func ReadSSE(r io.Reader, fn func(api.Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				var e api.Event
				if err := json.Unmarshal([]byte(data.String()), &e); err != nil {
					return err
				}
				data.Reset()
				if err := fn(e); err != nil {
					if err == ErrStop {
						return nil
					}
					return err
				}
			}
		case strings.HasPrefix(line, overflowComment):
			return &LagError{Last: strings.TrimSpace(strings.TrimPrefix(line, overflowComment))}
		case strings.HasPrefix(line, streamErrorComment+" "):
			return &StreamError{Message: strings.TrimPrefix(line, streamErrorComment+" ")}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return sc.Err()
}
