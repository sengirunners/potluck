package cfsetup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// PublicResolver is where Probe asks for names: a new hostname is often
// negatively cached by the local resolver for a while (seen with a fresh
// trycloudflare name), and setup wants the answer the world sees.
var PublicResolver = "1.1.1.1:53"

// Probe asks url as any stranger would (no key, no token), with a fresh
// nonce, and checks the answer as a member's transport does: signed over
// that nonce by the home key with fingerprint fp. A Cloudflare challenge or
// an edge error is told apart from it.
func Probe(ctx context.Context, url, fp string) (Answer, error) {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, PublicResolver)
	}}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Resolver: r}
	tr := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, ForceAttemptHTTP2: true}
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
	api.SetHeader(req.Header, tunnel.HeaderNonce, nonce)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return Answer{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	a := Answer{Status: resp.StatusCode, HomeKey: api.GetHeader(resp.Header, tunnel.HeaderHomeKey), ServerSays: strings.TrimSpace(string(firstLine(body)))}
	switch err := tunnel.VerifyAnswer(fp, nonce, resp.StatusCode, resp.Header, body); {
	case err == nil:
		a.Signed = true
	case errors.Is(err, tunnel.ErrHomeKey):
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

// ExecRunner runs the cloudflared at Path with a minimal environment (PATH,
// the user's, and env), its stderr streamed to out.
type ExecRunner struct{ Path string }

// Run implements Runner.
func (e ExecRunner) Run(ctx context.Context, env []string, out io.Writer, args ...string) ([]byte, error) {
	if e.Path == "" {
		return nil, errors.New("cloudflared is not provisioned in $ZM_HOME/tools (zm tunnel setup fetches the pinned one)")
	}
	cmd := exec.CommandContext(ctx, e.Path, args...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "USER=" + os.Getenv("USER")}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, out)
	cmd.Stderr = io.MultiWriter(&stderr, out)
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		return stdout.Bytes(), &RunError{Args: args, Err: err, Stderr: msg}
	}
	return stdout.Bytes(), nil
}

// RunError is a cloudflared call that failed, with what it said.
type RunError struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *RunError) Error() string {
	return "cloudflared " + strings.Join(e.Args, " ") + ": " + e.Err.Error() + ": " + e.Stderr
}
