package teamlink

// GET /v0/events for a joined project (SPEC 17.3). A replica has no copy of a
// project's event log, so the stream comes from the authority, filtered and
// redacted there for this machine's token, and ended there if that token's
// access changes. A project named goes to its authority, live or replay. With
// none named, a replay (live=0) is this machine's events followed by each joined
// project's: several logs, not one, so its ids do not make one cursor. A live
// stream with none named stays this machine's.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
)

// eventsRoute reports whether it answered r.
func (s *Service) eventsRoute(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	named := r.URL.Query().Get("project")
	if named == "" && api.GetHeader(r.Header, api.HeaderProjectFrom) != project.RuleEnv {
		named = api.GetHeader(r.Header, api.HeaderProject)
	}
	if named != "" {
		slug, area, _ := strings.Cut(named, "/")
		if _, local := s.d.Projects.Lookup(slug); local {
			return false
		}
		x := s.ByName(slug)
		if x == nil {
			return false // not found locally either: the router answers
		}
		resp, err := s.eventsFrom(r, x, area)
		if err != nil {
			api.WriteError(w, err)
			return true
		}
		defer resp.Body.Close()
		if err := s.streamEvents(w, r, resp, true); err != nil {
			failStream(w, x, err)
		}
		return true
	}
	if r.URL.Query().Get("live") != "0" {
		return false
	}
	st := &statusWriter{ResponseWriter: w}
	next.ServeHTTP(st, r)
	if st.status != http.StatusOK {
		return true // refused here (a bad cursor): the same call would be refused there
	}
	for _, x := range s.List() {
		if Ended(x.Snapshot()) != nil {
			continue // this machine is out of it (revoked, purged): as in the lists
		}
		resp, err := s.eventsFrom(r, x, "")
		if err != nil {
			failStream(w, x, err)
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			failStream(w, x, fmt.Errorf("it answered %d", resp.StatusCode))
		}
		err = s.streamEvents(w, r, resp, false)
		resp.Body.Close()
		if err != nil {
			failStream(w, x, err)
		}
	}
	return true
}

// failStream ends a stream that cannot be whole: the reason for a reader that
// reads it, and a dropped connection for one that does not, so a short stream
// never looks complete.
func failStream(w http.ResponseWriter, x *Remote, err error) {
	var mid *midFrame
	if !errors.As(err, &mid) { // inside a frame the reason would read as its data
		events.WriteStreamError(w, fmt.Errorf("the events of %s stopped partway: %v", x.Slug(), err))
		_ = http.NewResponseController(w).Flush()
	}
	panic(http.ErrAbortHandler)
}

// midFrame is a stream cut inside an event's frame.
type midFrame struct{ error }

func (e *midFrame) Unwrap() error { return e.error }

// eventsFrom asks x's authority for its project's events.
func (s *Service) eventsFrom(r *http.Request, x *Remote, area string) (*http.Response, error) {
	g := r.Clone(r.Context())
	g.Body, g.ContentLength = http.NoBody, 0
	q := g.URL.Query()
	q.Set("project", x.Slug()) // forwardResp names the authority's id for it
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		q.Set("since", id) // forwardResp sends its own headers
	}
	g.URL.RawQuery = q.Encode()
	return s.ForwardResp(g, x, area, true, []byte{})
}

// streamEvents copies an event stream as it arrives. head says w has no status
// yet. Only the reader leaving ends it quietly: the forward's own timeout also
// cancels, and that is a stream cut short. Time spent writing to a slow reader
// is not the authority's silence.
func (s *Service) streamEvents(w http.ResponseWriter, r *http.Request, resp *http.Response, head bool) error {
	// A daemon stopping ends the stream as a local one ends, rather than hold
	// its shutdown.
	done := make(chan struct{})
	defer close(done)
	stopping := make(chan struct{})
	go func() {
		select {
		case <-s.d.Closing:
			close(stopping)
			resp.Body.Close()
		case <-done:
		}
	}()
	if head {
		if v := resp.Header.Get("Content-Type"); v != "" {
			w.Header().Set("Content-Type", v)
		}
		w.WriteHeader(resp.StatusCode)
	}
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	wb, _ := resp.Body.(*watchedBody)
	live := r.URL.Query().Get("live") != "0"
	buf := make([]byte, 32<<10)
	whole := true // the bytes so far end on a frame's end
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			whole = n >= 2 && buf[n-2] == '\n' && buf[n-1] == '\n'
			if wb != nil {
				wb.timer.Stop()
			}
			_, werr := w.Write(buf[:n])
			if werr == nil {
				_ = rc.Flush()
			}
			if wb != nil {
				wb.timer.Reset(CallTimeout)
			}
			if werr != nil {
				return nil // the reader left
			}
		}
		select {
		case <-stopping:
			switch {
			case r.Context().Err() != nil:
				return nil // the reader left
			case !whole:
				return &midFrame{errors.New("the daemon is stopping")}
			case live:
				_, _ = io.WriteString(w, ": daemon stopping\n\n")
				_ = rc.Flush()
				return nil
			}
			return errors.New("the daemon is stopping") // a replay it cut short is no whole replay
		default:
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if r.Context().Err() != nil {
				return nil // the reader left
			}
			if !whole {
				return &midFrame{err}
			}
			return err
		}
	}
}

// statusWriter notes the status a handler answered, and passes everything on.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }
