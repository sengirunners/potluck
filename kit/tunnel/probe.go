package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

// PublicResolver is where Probe asks for names first: a new hostname is
// often negatively cached by the local resolver for a while (seen with a
// fresh trycloudflare name), and the answer wanted is the one the world
// sees. When that lookup fails (a network that blocks it), the system's
// resolver is asked.
var PublicResolver = "1.1.1.1:53"

// ReachWithin bounds Reachable: well under a forwarded call's timeout
// (teamlink.CallTimeout), so an invite a member machine forwards is
// answered, never given up on and asked again.
var ReachWithin = 8 * time.Second

// Answer is one outside look at the home.
type Answer struct {
	Status int
	// Signed: the answer is signed, over the nonce Probe sent, by the home
	// key with the fingerprint asked for (as a member's transport checks).
	Signed bool
	// OtherHome: an answer signed by another key, or a forged signature.
	OtherHome  bool
	HomeKey    string // the home key header as sent (information only)
	Challenge  bool   // Cloudflare answered with a challenge (Bot Fight Mode)
	EdgeError  bool   // Cloudflare could not reach the tunnel (530, 502)
	ServerSays string
}

// Problem is why a member could not use the home behind this answer, and
// what fixes it, in plain words; why is "" when the home answered signed by
// its key.
func (a Answer) Problem() (why, fix string) {
	switch {
	case a.Signed:
		return "", ""
	case a.OtherHome:
		return "another " + config.Program + " home answers there, or one with a signature this home's key did not make",
			"the hostname points elsewhere: point its DNS record (or the tunnel's route) at this machine's tunnel"
	case a.Challenge:
		return "Cloudflare answers with a challenge (Bot Fight Mode), which members' machines cannot pass",
			"turn off Bot Fight Mode for this hostname in Cloudflare's dashboard (Security, Bots)"
	case a.EdgeError:
		return fmt.Sprintf("Cloudflare cannot reach this machine's cloudflared (status %d)", a.Status),
			"check cloudflared is running on this machine (a quick tunnel's name changes each time cloudflared restarts: set the tunnel up again)"
	}
	return fmt.Sprintf("it does not answer as this home (status %d)", a.Status),
		"check the hostname reaches this machine's cloudflared and that cloudflared forwards to this daemon's tunnel listener"
}

// Probe asks url as any stranger would (no key, no token, nothing of any
// invite), with a fresh nonce, and checks the answer as a member's
// transport does: signed over that nonce by the home key with fingerprint
// fp. A Cloudflare challenge or an edge error is told apart from it.
func Probe(ctx context.Context, url, fp string) (Answer, error) {
	public := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, PublicResolver)
	}}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil || net.ParseIP(host) != nil {
			return dialer.DialContext(ctx, network, addr)
		}
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		ips, err := public.LookupHost(lctx, host)
		cancel()
		if err != nil || len(ips) == 0 {
			if ips, err = net.DefaultResolver.LookupHost(ctx, host); err != nil {
				return nil, err
			}
		}
		var last error
		for _, ip := range ips {
			c, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
	tr := &http.Transport{DialContext: dial, TLSHandshakeTimeout: 10 * time.Second, ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Answer{}, err
	}
	nb := make([]byte, 16)
	_, _ = rand.Read(nb)
	nonce := base64.RawURLEncoding.EncodeToString(nb)
	api.SetHeader(req.Header, HeaderNonce, nonce)
	resp, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return Answer{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	a := Answer{Status: resp.StatusCode, HomeKey: api.GetHeader(resp.Header, HeaderHomeKey), ServerSays: strings.TrimSpace(string(firstLine(body)))}
	switch err := VerifyAnswer(fp, nonce, resp.StatusCode, resp.Header, body); {
	case err == nil:
		a.Signed = true
	case errors.Is(err, ErrHomeKey):
		a.OtherHome = true
	}
	ct := resp.Header.Get("Content-Type")
	switch {
	case resp.Header.Get("Cf-Mitigated") == "challenge",
		a.HomeKey == "" && strings.HasPrefix(ct, "text/html") && (resp.StatusCode == 403 || resp.StatusCode == 503) &&
			bytes.Contains(body, []byte("challenge")):
		a.Challenge = true
	case a.HomeKey == "" && (resp.StatusCode == 530 || resp.StatusCode == 502 || resp.StatusCode == 504 || resp.StatusCode == 1033):
		a.EdgeError = true
	}
	return a, nil
}

func firstLine(b []byte) []byte {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return b
}

// Reachable says whether the home at url, pinned by fp, answers from
// outside as that home now: nil only for an answer signed by its key over a
// fresh nonce. Anything else (no answer in ReachWithin, an edge error,
// another home, a check that cannot be made) is an error in plain words
// with code unavailable, as core.RouteDown: the tunnel is set up but does
// not work now. It sends nothing secret and makes nothing.
func Reachable(ctx context.Context, url, fp string) error {
	ctx, cancel := context.WithTimeout(ctx, ReachWithin)
	defer cancel()
	a, err := Probe(ctx, strings.TrimRight(url, "/")+"/v0/health", fp)
	why, fix := "", ""
	if err != nil {
		why, fix = "it did not answer: "+err.Error(), "check this machine is online and cloudflared is running on it"
	} else {
		why, fix = a.Problem()
	}
	if why == "" {
		return nil
	}
	return core.RouteDown{Err: api.Errorf(api.CodeUnavailable, "the tunnel %s does not reach this machine now: %s", url, why).WithHint(fix)}
}

// Reachable checks this daemon's own tunnel URL from outside (Reachable),
// the route an https invite names, and returns that URL and this home's
// key fingerprint as checked.
func (s *Service) Reachable(ctx context.Context) (url, fp string, err error) {
	u := s.URL()
	if u == "" {
		return "", "", errNoURL()
	}
	fp, err = s.Fingerprint()
	if err != nil {
		return "", "", core.RouteDown{Err: api.Errorf(api.CodeUnavailable, "the tunnel cannot be checked: this home's key cannot be read: %v", err)}
	}
	if err := Reachable(ctx, u, fp); err != nil {
		return "", "", err
	}
	return u, fp, nil
}

// errNoURL: no tunnel is set up on this machine.
func errNoURL() error {
	setup := `set "tunnel": {"listen": "127.0.0.1:7435", "url": "https://<host>"} in config.json, restart the daemon, and run cloudflared`
	if !config.Missing("tunnel setup") {
		setup = "run " + config.Command("tunnel setup", "<hostname>")
	}
	return api.Errorf(api.CodeUnavailable, "this machine has no tunnel").
		WithHint(setup + " for an https code (they need no Tailscale), or " + config.Command("tailnet up") + " for a tailnet one")
}
