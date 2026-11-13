package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

type recoveryRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f recoveryRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// legacyClient signs and verifies exactly once, as a member without recovery
// negotiation does. It deliberately never echoes a recovery challenge.
func legacyClient(c *http.Client) *http.Client {
	out := *c
	tr := c.Transport.(*transport)
	out.Transport = recoveryRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil {
			var err error
			body, err = io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				return nil, err
			}
		}
		return tr.roundTrip(r, body, "")
	})
	return &out
}

// recoveryHome retains the original home key but loses its nonce journal,
// just as an off-site restore with the key supplied separately does.
func recoveryHome(t *testing.T, damage string) *Service {
	t.Helper()
	h := newService(t)
	if _, err := h.Fingerprint(); err != nil {
		t.Fatal(err)
	}
	if damage == "unreadable" {
		if err := os.WriteFile(h.journal.path, bytes.Repeat([]byte("x"), 2*maxLine), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restored := restartService(t, h)
	t.Cleanup(func() { restored.Close() })
	return restored
}

type recoveryRequest struct {
	method, uri string
	header      http.Header
	body        []byte
}

func observeRecovery(next http.Handler, record func(recoveryRequest)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body", http.StatusBadRequest)
			return
		}
		record(recoveryRequest{r.Method, r.URL.RequestURI(), r.Header.Clone(), body})
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

func replayRecovery(t *testing.T, u string, old recoveryRequest, alter func(http.Header)) *http.Response {
	t.Helper()
	r, err := http.NewRequest(old.method, u+old.uri, bytes.NewReader(old.body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = old.header.Clone()
	if alter != nil {
		alter(r.Header)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestRecoveryChallengePreservesWriteAndLegacyHold(t *testing.T) {
	for _, damage := range []string{"missing", "unreadable"} {
		t.Run(damage, func(t *testing.T) {
			h := recoveryHome(t, damage)
			fp, _ := h.Fingerprint()
			_, c := member(t, fp)
			var calls atomic.Int32
			var mu sync.Mutex
			var requests []recoveryRequest
			handler := h.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if get(r.Header, HeaderRecovery) != "" {
					t.Error("transport challenge reached the route")
				}
				w.WriteHeader(http.StatusCreated)
			}))
			srv := httptest.NewServer(observeRecovery(handler, func(r recoveryRequest) {
				mu.Lock()
				requests = append(requests, r)
				mu.Unlock()
			}))
			t.Cleanup(srv.Close)
			body := "the exact non-seekable request body"
			r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v0/work/x/comment?project=p", io.NopCloser(strings.NewReader(body)))
			for k, v := range map[string]string{
				"Authorization": "Bearer zm_tok", "Content-Type": "application/json", api.HeaderWriteID: "stable-write-id",
				api.HeaderProject: "project-id", api.HeaderAs: "member/session", api.HeaderScopeVersion: "2",
				HeaderRecovery: "caller-supplied-stale-challenge",
			} {
				r.Header.Set(k, v)
			}
			tr := c.Transport.(*transport)
			tr.extra = http.Header{"Cf-Access-Client-Id": {"access-id"}, "Cf-Access-Client-Secret": {"access-secret"}}
			original := r.Header.Clone()
			resp, err := c.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated || calls.Load() != 1 {
				t.Fatalf("recovery status %d, route calls %d", resp.StatusCode, calls.Load())
			}
			if !reflect.DeepEqual(r.Header, original) {
				t.Fatal("retry mutated caller headers")
			}
			mu.Lock()
			got := append([]recoveryRequest(nil), requests...)
			mu.Unlock()
			if len(got) != 2 {
				t.Fatalf("recovery made %d attempts, want2", len(got))
			}
			if get(got[0].header, HeaderRecovery) != "" || recoveryValue(got[1].header) == "" ||
				get(got[0].header, HeaderNonce) == get(got[1].header, HeaderNonce) || get(got[0].header, HeaderSig) == get(got[1].header, HeaderSig) {
				t.Fatalf("attempts did not negotiate a fresh signed challenge: %+v", got)
			}
			for _, attempt := range got {
				if attempt.method != r.Method || attempt.uri != r.URL.RequestURI() || string(attempt.body) != body {
					t.Fatalf("retry changed the request: %+v", attempt)
				}
				for k, vs := range original {
					if k != HeaderRecovery && !reflect.DeepEqual(attempt.header.Values(k), vs) {
						t.Fatalf("retry changed %s: %v, want %v", k, attempt.header.Values(k), vs)
					}
				}
				if attempt.header.Get("Cf-Access-Client-Id") != "access-id" || attempt.header.Get("Cf-Access-Client-Secret") != "access-secret" {
					t.Fatal("retry lost access headers")
				}
			}
			if _, err := os.Stat(h.journal.path + ".hold"); err != nil || !h.journal.degraded() {
				t.Fatal("successful recovery write cleared the legacy hold", err)
			}
			if resp, b, err := do(t, legacyClient(c), "POST", srv.URL+"/v0/work/x/comment", "zm_tok", "legacy"); err != nil || resp.StatusCode != 503 || recoveryValue(resp.Header) == "" {
				t.Fatalf("legacy write was not held: %v %s", err, b)
			}
			if calls.Load() != 1 {
				t.Fatal("legacy write reached the route")
			}
			if resp, _, err := do(t, c, "GET", srv.URL+"/v0/x", "zm_tok", ""); err != nil || resp.StatusCode != http.StatusCreated {
				t.Fatalf("read during recovery: %v", err)
			}
			mu.Lock()
			last := requests[len(requests)-1]
			count := len(requests)
			mu.Unlock()
			if count != 4 || last.method != "GET" || get(last.header, HeaderRecovery) != "" {
				t.Fatalf("read needed recovery negotiation: %d %+v", count, last)
			}
			// The original fallback remains available after the entire local
			// safety window, even to clients which cannot negotiate.
			h.journal.mu.Lock()
			h.journal.degradedTo = time.Now().Add(-time.Second)
			h.journal.mu.Unlock()
			if resp, b, err := do(t, legacyClient(c), "POST", srv.URL+"/v0/work/x/comment", "zm_tok", "after window"); err != nil || resp.StatusCode != 201 {
				t.Fatalf("legacy write after local window: %v %s", err, b)
			}
			if _, err := os.Stat(h.journal.path + ".hold"); !os.IsNotExist(err) {
				t.Fatalf("completed local window retained recovery marker: %v", err)
			}
		})
	}
}

func TestRecoveryChallengeRejectsCapturedRequests(t *testing.T) {
	h := recoveryHome(t, "missing")
	fp, _ := h.Fingerprint()
	_, c := member(t, fp)
	var calls atomic.Int32
	var mu sync.Mutex
	var captured []recoveryRequest
	route := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(201) })
	srv := httptest.NewServer(observeRecovery(h.Handler(route), func(r recoveryRequest) {
		mu.Lock()
		captured = append(captured, r)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	if resp, _, err := do(t, c, "POST", srv.URL+"/v0/work/x/comment", "zm_tok", "once"); err != nil || resp.StatusCode != 201 {
		t.Fatalf("fresh recovery: %v", err)
	}
	mu.Lock()
	first, accepted := captured[0], captured[1]
	mu.Unlock()
	if resp := replayRecovery(t, srv.URL, accepted, nil); resp.StatusCode != 400 || calls.Load() != 1 {
		t.Fatalf("same-process replay: %d, calls %d", resp.StatusCode, calls.Load())
	}
	if resp := replayRecovery(t, srv.URL, first, func(h http.Header) {
		h.Del(HeaderRecovery)
		h.Set("Potluck-Recovery", get(accepted.header, HeaderRecovery))
	}); resp.StatusCode != 401 || calls.Load() != 1 {
		t.Fatalf("appending challenge without signing: %d, calls %d", resp.StatusCode, calls.Load())
	}
	srv.Close()
	h.Close()
	// Lose the rebuilt journal again. The durable hold survives and the
	// second process cannot accept either captured form of the old request.
	if err := os.Remove(h.journal.path); err != nil {
		t.Fatal(err)
	}
	h2 := restartService(t, h)
	t.Cleanup(func() { h2.Close() })
	srv2 := httptest.NewServer(h2.Handler(route))
	t.Cleanup(srv2.Close)
	for _, old := range []recoveryRequest{first, accepted} {
		if resp := replayRecovery(t, srv2.URL, old, nil); resp.StatusCode != 503 || recoveryValue(resp.Header) == "" || calls.Load() != 1 {
			t.Fatalf("pre-restart replay: %d, calls %d", resp.StatusCode, calls.Load())
		}
	}
	if next, _ := h2.recoveryChallenge(); next == get(accepted.header, HeaderRecovery) {
		t.Fatal("recovery challenge survived restart")
	}
}

func signedRecoveryReply(t *testing.T, h *Service, r *http.Request, w http.ResponseWriter, headers http.Header) {
	t.Helper()
	key, err := h.key()
	if err != nil {
		t.Fatal(err)
	}
	bw := &bufWriter{h: headers.Clone()}
	bw.WriteHeader(http.StatusServiceUnavailable)
	io.WriteString(bw, `{"error":{"code":"unavailable","message":"recovery"}}`)
	bw.finish(w, key, get(r.Header, HeaderNonce), "")
}

func TestRecoveryChallengeRetriesOnlyVerifiedRefusalOnce(t *testing.T) {
	for _, mode := range []string{"bounded", "unsigned", "wrong-key", "tampered", "duplicate", "malformed", "ordinary", "read", "no-token", "cancelled", "disconnected", "short-answer"} {
		t.Run(mode, func(t *testing.T) {
			h := newService(t)
			fp, _ := h.Fingerprint()
			other := newService(t)
			challenge, _ := h.recoveryChallenge()
			_, c := member(t, fp)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if mode == "disconnected" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
					return
				}
				headers := http.Header{HeaderRecovery: {challenge}}
				switch mode {
				case "unsigned":
					w.Header().Set(HeaderRecovery, challenge)
					w.WriteHeader(503)
					return
				case "wrong-key":
					signedRecoveryReply(t, other, r, w, headers)
					return
				case "duplicate":
					headers.Add(HeaderRecovery, challenge)
				case "malformed":
					headers.Set(HeaderRecovery, "not-a-challenge")
				case "ordinary":
					headers.Del(HeaderRecovery)
				case "cancelled":
					cancel()
				}
				if mode == "tampered" || mode == "short-answer" {
					rec := httptest.NewRecorder()
					signedRecoveryReply(t, h, r, rec, headers)
					for k, v := range rec.Header() {
						w.Header()[k] = v
					}
					if mode == "tampered" {
						w.Header().Set(HeaderRecovery, base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
					} else {
						w.Header().Set("Content-Length", fmt.Sprint(rec.Body.Len()+10))
					}
					w.WriteHeader(rec.Code)
					w.Write(rec.Body.Bytes())
					return
				}
				signedRecoveryReply(t, h, r, w, headers)
			}))
			t.Cleanup(srv.Close)
			method := http.MethodPost
			if mode == "read" {
				method = http.MethodGet
			}
			r, _ := http.NewRequestWithContext(ctx, method, srv.URL+"/v0/work/x/comment", nil)
			if mode != "no-token" {
				r.Header.Set("Authorization", "Bearer zm_tok")
			}
			resp, err := c.Do(r)
			if resp != nil {
				resp.Body.Close()
			}
			want := int32(1)
			if mode == "bounded" {
				want = 2
			}
			if got := attempts.Load(); got != want {
				t.Fatalf("%s made %d attempts, want %d (error %v)", mode, got, want, err)
			}
			if mode == "unsigned" || mode == "wrong-key" || mode == "tampered" || mode == "disconnected" || mode == "short-answer" {
				if err == nil {
					t.Fatalf("%s answer was trusted", mode)
				}
			} else if mode != "cancelled" && (err != nil || resp.StatusCode != 503) {
				t.Fatalf("refusal result: %v, response %+v", err, resp)
			}
		})
	}
}

func TestRecoveryChallengeCannotComeFromRoute(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprint(held), func(t *testing.T) {
			h := newService(t)
			if held {
				h = recoveryHome(t, "missing")
			}
			t.Cleanup(func() { h.Close() })
			fp, _ := h.Fingerprint()
			challenge, _ := h.recoveryChallenge()
			_, c := member(t, fp)
			var calls atomic.Int32
			var attempts atomic.Int32
			srv := httptest.NewServer(observeRecovery(h.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set(HeaderRecovery, challenge)
				w.WriteHeader(503) // an application refusal after dispatch is not safe to retry
			})), func(recoveryRequest) { attempts.Add(1) }))
			t.Cleanup(srv.Close)
			resp, _, err := do(t, c, "POST", srv.URL+"/v0/work/x/comment", "zm_tok", "")
			want := int32(1)
			if held {
				want = 2
			}
			if err != nil || resp.StatusCode != 503 || get(resp.Header, HeaderRecovery) != "" || calls.Load() != 1 || attempts.Load() != want {
				t.Fatalf("application503 triggered retry: %v, calls %d, attempts %d", err, calls.Load(), attempts.Load())
			}
		})
	}
}

func TestRecoveryChallengeNoTokenRedemptionKeepsHold(t *testing.T) {
	h := recoveryHome(t, "missing")
	fp, _ := h.Fingerprint()
	_, c := member(t, fp)
	challenge, _ := h.recoveryChallenge()
	var calls atomic.Int32
	srv := httptest.NewServer(h.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(201)
	})))
	t.Cleanup(srv.Close)
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v0/invites/redeem", nil)
	resp, err := c.Transport.(*transport).roundTrip(r, nil, challenge)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || get(resp.Header, HeaderRecovery) != "" || calls.Load() != 0 {
		t.Fatalf("no-token redemption bypassed hold: %d, calls %d", resp.StatusCode, calls.Load())
	}
}

func TestRecoveryChallengeRequiresJournalAndBudget(t *testing.T) {
	for _, mode := range []string{"journal", "marker", "budget"} {
		t.Run(mode, func(t *testing.T) {
			h := recoveryHome(t, "missing")
			fp, _ := h.Fingerprint()
			m, c := member(t, fp)
			if mode == "marker" {
				if err := os.MkdirAll(h.journal.path+".hold.tmp", 0o700); err != nil {
					t.Fatal(err)
				}
			}
			h.journal.load(time.Now())
			if mode == "journal" {
				h.journal.close()
				if err := os.Remove(h.journal.path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(h.journal.path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "budget" {
				kid, _ := m.KeyID()
				h.journal.mu.Lock()
				seen := map[string]int64{}
				for i := 0; i < MaxNoncesPerKey; i++ {
					seen[fmt.Sprint(i)] = time.Now().Add(time.Hour).Unix()
				}
				h.journal.keys[kid] = seen
				h.journal.live = len(seen)
				h.journal.mu.Unlock()
			}
			var calls atomic.Int32
			var attempts atomic.Int32
			srv := httptest.NewServer(observeRecovery(h.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(201)
			})), func(recoveryRequest) { attempts.Add(1) }))
			t.Cleanup(srv.Close)
			resp, _, err := do(t, c, "POST", srv.URL+"/v0/work/x/comment", "zm_tok", "")
			if err != nil || resp.StatusCode != 503 || get(resp.Header, HeaderRecovery) != "" || calls.Load() != 0 || attempts.Load() != 1 {
				t.Fatalf("%s failure bypassed: %v, calls %d, attempts %d", mode, err, calls.Load(), attempts.Load())
			}
		})
	}
}

func TestRecoveryChallengeConcurrentWrites(t *testing.T) {
	h := recoveryHome(t, "missing")
	fp, _ := h.Fingerprint()
	_, c := member(t, fp)
	var calls atomic.Int32
	srv := httptest.NewServer(h.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(201)
	})))
	t.Cleanup(srv.Close)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v0/work/x/comment", nil)
			r.Header.Set("Authorization", "Bearer zm_tok")
			resp, err := c.Do(r)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != 201 {
				t.Errorf("concurrent recovery status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 8 {
		t.Fatalf("concurrent recovery dispatched %d writes", calls.Load())
	}
}

// Keep this compile-time assertion next to the legacy helper: it must test
// the same signed one-attempt path, not bypass the door with a plain client.
var _ http.RoundTripper = recoveryRoundTripperFunc(nil)
