package vmhost

// Admission (binding key admission_url; zm-eakh.12.7.1, plan D2): a host
// that runs other work beside zm (a CI pool, its owner's jobs) may name a
// controller zm must ask for room before every VM. The contract is generic,
// JSON over HTTP, and nothing in it names a host:
//
//	POST   <url>/leases              {"vcpu", "memory_mib", "holder", "ttl_s"}
//	       201 (or 200 for a repeat of the same holder) {"lease", "expires_at"}
//	       409 {"reason", "retry_s"}: no room now; zm's acquire answers unavailable with both
//	POST   <url>/leases/{lease}/renew {"ttl_s"}
//	       200 {"expires_at"}; 404 or 410: the controller wants the room back, and zm ends
//	       the VM as reclaimed (never billed)
//	DELETE <url>/leases/{lease}      204, or 404 when it is gone already: both fine
//
// holder is "zm:<target>/<run>", unique per run, so a retried POST answers
// the same lease. Leases are short (admission_ttl_s, default 120 s) and the
// run's supervisor renews at a third of that, so a dead supervisor frees its
// room within one TTL. A lease whose renewals fail until it lapses ends the
// VM too: zm never runs on room the controller may have handed on. With
// admission_token_file (a 0600 file) every call carries "Authorization:
// Bearer <token>". The supervisor makes every call, so the lease holds while
// zm is down, as the TTL does.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultAdmissionTTL = 120

// admission is a host's admission controller.
type admission struct {
	url, token string
	ttl        int
	hc         *http.Client
}

// noRoom is a 409: the controller has no room now.
type noRoom struct {
	reason string
	retry  int
}

func (e noRoom) Error() string {
	if e.retry > 0 {
		return fmt.Sprintf("no room on this host now: %s (retry in %d s)", e.reason, e.retry)
	}
	return "no room on this host now: " + e.reason
}

// errLeaseGone: the controller took the room back (404 or 410 on renew).
var errLeaseGone = errors.New("the host's admission controller took the room back")

func newAdmission(c config) (*admission, error) {
	if c.AdmissionURL == "" {
		return nil, nil
	}
	// No redirects: a 3xx would turn a POST or DELETE into something else,
	// or send it elsewhere; it surfaces as an error instead.
	hc := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	a := &admission{url: strings.TrimRight(c.AdmissionURL, "/"), ttl: c.AdmissionTTL, hc: hc}
	if c.AdmissionTokenFile != "" {
		tok, err := readTokenFile(c.AdmissionTokenFile)
		if err != nil {
			return nil, err
		}
		a.token = tok
	}
	return a, nil
}

// readTokenFile reads a 0600 token file, its mode checked on the handle read.
func readTokenFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("admission_token_file: %v", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("admission_token_file: %v", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("admission_token_file %s must be 0600", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 8<<10))
	if err != nil {
		return "", fmt.Errorf("admission_token_file: %v", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (a *admission) call(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.url+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("admission controller: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if out != nil && len(b) > 0 && resp.StatusCode < 500 {
		if err := json.Unmarshal(b, out); err != nil && resp.StatusCode < 300 {
			return resp.StatusCode, fmt.Errorf("admission controller: %s %s answered %d with no JSON: %v", method, path, resp.StatusCode, err)
		}
	}
	return resp.StatusCode, nil
}

type admissionLease struct {
	Lease     string    `json:"lease"`
	ExpiresAt time.Time `json:"expires_at"`
}

// take asks for room for one VM.
func (a *admission) take(ctx context.Context, vcpu, memoryMiB int, holder string) (admissionLease, error) {
	var out struct {
		admissionLease
		Reason string `json:"reason"`
		Retry  int    `json:"retry_s"`
	}
	code, err := a.call(ctx, http.MethodPost, "/leases",
		map[string]any{"vcpu": vcpu, "memory_mib": memoryMiB, "holder": holder, "ttl_s": a.ttl}, &out)
	switch {
	case err != nil:
		return admissionLease{}, err
	case code == http.StatusConflict:
		if out.Reason == "" {
			out.Reason = "the controller gave no reason"
		}
		return admissionLease{}, noRoom{out.Reason, out.Retry}
	case code != http.StatusCreated && code != http.StatusOK:
		return admissionLease{}, fmt.Errorf("admission controller: POST /leases answered %d", code)
	case out.Lease == "" || out.ExpiresAt.IsZero():
		return admissionLease{}, fmt.Errorf("admission controller: POST /leases answered no lease or expires_at")
	}
	return out.admissionLease, nil
}

// renew extends a lease: errLeaseGone when the controller took it back.
func (a *admission) renew(ctx context.Context, lease string) (time.Time, error) {
	var out struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	code, err := a.call(ctx, http.MethodPost, "/leases/"+url.PathEscape(lease)+"/renew", map[string]any{"ttl_s": a.ttl}, &out)
	switch {
	case err != nil:
		return time.Time{}, err
	case code == http.StatusNotFound || code == http.StatusGone:
		return time.Time{}, errLeaseGone
	case code != http.StatusOK || out.ExpiresAt.IsZero():
		return time.Time{}, fmt.Errorf("admission controller: renew answered %d", code)
	}
	return out.ExpiresAt, nil
}

// release gives the room back; a lease already gone is fine.
func (a *admission) release(ctx context.Context, lease string) error {
	code, err := a.call(ctx, http.MethodDelete, "/leases/"+url.PathEscape(lease), nil, nil)
	switch {
	case err != nil:
		return err
	case code == http.StatusNoContent || code == http.StatusOK || code == http.StatusNotFound || code == http.StatusGone:
		return nil
	}
	return fmt.Errorf("admission controller: DELETE answered %d", code)
}

// held is a supervisor's lease: renewed at a third of its TTL from its own
// goroutine (keepAlive), so a slow controller never delays the supervisor's
// deadline or reclaim checks, and lost once the controller takes it back or
// it lapses unrenewed.
type held struct {
	a              *admission
	lease          admissionLease
	renewed, tried time.Time
	lost           chan struct{} // closed when the lease is lost; err says why
	err            error
	done           chan struct{}         // closed when keepAlive returns
	confirmed      func(time.Time) error // a host guard may extend only on confirmed renewals
}

// keepAlive renews until ctx ends or the lease is lost.
func (h *held) keepAlive(ctx context.Context) {
	defer close(h.done)
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := h.keep(ctx, now); err != nil {
				h.err = err
				close(h.lost)
				return
			}
		}
	}
}

// gone answers why the lease is lost, nil while it holds.
func (h *held) gone() error {
	if h == nil {
		return nil
	}
	select {
	case <-h.lost:
		return h.err
	default:
		return nil
	}
}

// renewRetry spaces renewals that failed while time is left.
var renewRetry = 5 * time.Second

// due says whether a renewal is due.
func (h *held) due(now time.Time) bool {
	return now.Sub(h.renewed) >= time.Duration(h.a.ttl)*time.Second/3 && now.Sub(h.tried) >= renewRetry
}

// keep renews when due; an error means the VM must end (taken back, or
// lapsed while the controller did not answer).
func (h *held) keep(ctx context.Context, now time.Time) error {
	if !h.due(now) {
		return nil
	}
	h.tried = now
	// A renewal waits at most 5 s, and never past the lease's end.
	wait := min(5*time.Second, time.Until(h.lease.ExpiresAt))
	if wait < time.Second {
		wait = time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, wait)
	exp, err := h.a.renew(rctx, h.lease.Lease)
	cancel()
	switch {
	case err == nil:
		if h.confirmed != nil {
			if err := h.confirmed(exp); err != nil {
				return fmt.Errorf("host deadline renewal: %w", err)
			}
		}
		h.lease.ExpiresAt, h.renewed = exp, now
		return nil
	case errors.Is(err, errLeaseGone):
		return err
	case now.After(h.lease.ExpiresAt):
		return fmt.Errorf("the admission lease lapsed: %v", err)
	}
	return nil // a failed renewal with time left: try again next tick
}
