// Package cfsetup is zm tunnel setup (SPEC 17.11 "Your own domain"): it
// puts a team's home at https://<hostname> on the team's own Cloudflare zone
// with one command. It drives cloudflared (a pinned download, internal/tools)
// through its own login, a named tunnel and a DNS route, writes cloudflared's
// config under $ZM_HOME, keeps `cloudflared tunnel run` alive with the
// machine's service manager, starts zm's tunnel listener, and checks the
// result from outside. Every cloud and local step is recorded in a state
// file, so a re-run picks up where the last one stopped and a failure undoes
// what this run made.
//
// Nothing here needs a Cloudflare API token: the login certificate creates
// the tunnel and its DNS record; zm's answers carry Cache-Control: no-store
// (no cache rule); the edge secret rides in the Host cloudflared sets
// (originRequest.httpHostHeader, no Transform Rule). The one zone setting
// zm cannot change is Bot Fight Mode: verification detects its challenge and
// prints the dashboard steps.
//
// Why our own driver and not a library: cloudflared is the maintained tool
// (rule 4), and this package only sequences its CLI and zm's own routes.
package cfsetup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// Dir is the setup's directory under $ZM_HOME (0700): cloudflared's login
// certificate (an account credential for the zone, under home/), the
// tunnel's credentials, its config, its log and the state file.
func Dir(zmHome string) string { return filepath.Join(zmHome, "tunnel", "cloudflared") }

// DefaultListen is the tunnel listener's address when none is given.
const DefaultListen = "127.0.0.1:7435"

// Runner runs cloudflared with args (env added to a minimal environment),
// streaming its output to out and returning its stdout.
type Runner interface {
	Run(ctx context.Context, env []string, out io.Writer, args ...string) ([]byte, error)
}

// Unit is cloudflared kept running by the service manager.
type Unit struct {
	Label string
	Args  []string
	Dir   string
	Log   string
	Env   map[string]string
}

// Supervisor is the machine's service manager (launchd on macOS, systemd
// --user on Linux: the one zm's tracker homes use).
type Supervisor interface {
	Name() string
	Install(ctx context.Context, u Unit) error
	// Restart makes a running unit start over (and so reread its config).
	Restart(ctx context.Context, u Unit) error
	Remove(ctx context.Context, u Unit) error
	Path(u Unit) string
}

// Daemon is zm's own tunnel routes on this machine's socket.
type Daemon interface {
	Tunnel(ctx context.Context) (api.TunnelStatus, error)
	TunnelUp(ctx context.Context, req api.TunnelUpRequest) (api.TunnelStatus, error)
	TunnelDown(ctx context.Context) (api.TunnelStatus, error)
}

// Setup is one zm tunnel setup, status or remove.
type Setup struct {
	ZmHome        string
	Hostname      string
	PreviewDomain string // Existing owned wildcard domain for sandbox previews.
	Listen        string // default DefaultListen
	Name          string // the tunnel's name; default from the hostname
	CF            Runner
	CFPath        string // cloudflared's absolute path, for the service
	Sup           Supervisor
	Zm            Daemon
	Out           io.Writer
	// Probe answers what https://<hostname>/v0/health says from outside,
	// checked against the home key fingerprint fp (default: Probe, through a
	// public resolver).
	Probe func(ctx context.Context, url, fp string) (Answer, error)
	// VerifyFor bounds waiting for the first good answer (DNS, the edge).
	VerifyFor time.Duration
	// Routes go before the door's rule on the same hostname: a path (a regex
	// cloudflared matches unanchored against the decoded path: anchor it)
	// another local listener answers unsigned. The
	// runner broker's public routes (runners.listen, SPEC 20) are the only
	// such; the door still takes everything else.
	Routes []Route
	// Linger reports whether this user's systemd services run with nobody
	// logged in (Linux; nil skips the check).
	Linger func() (bool, error)
}

// State is what setup made, saved after every step (state.json).
type State struct {
	Hostname      string `json:"hostname"`
	PreviewDomain string `json:"preview_domain,omitempty"`
	Name          string `json:"name"`
	TunnelID      string `json:"tunnel_id,omitempty"`
	Created       bool   `json:"created,omitempty"` // this machine created the tunnel (remove deletes it)
	Routed        bool   `json:"routed,omitempty"`  // the DNS record points at it
	Listen        string `json:"listen,omitempty"`
	EdgeSecret    string `json:"edge_secret,omitempty"`
	// EdgeHost is the Host cloudflared sends with the edge secret, chosen
	// when the secret was made: potluck-edge-<secret> for a setup made by
	// this build; "" in an older setup's state, which keeps zm-edge-<secret>.
	EdgeHost string `json:"edge_host,omitempty"`
	Label    string `json:"label,omitempty"`
	// Quick: a rehearsal on a temporary trycloudflare name (RunQuick): no
	// login, no account resources; the name changes on every start.
	Quick   bool      `json:"quick,omitempty"`
	Updated time.Time `json:"updated"`
}

// Answer is one outside look at the home.
type Answer struct {
	Status int
	// Signed: the answer is signed, over the nonce Probe sent, by the home
	// key with the fingerprint asked for (as a member's transport checks).
	Signed bool
	// OtherHome: a zm answer signed by another key, or a forged signature.
	OtherHome  bool
	HomeKey    string // Zm-Home-Key as sent (information only)
	Challenge  bool   // Cloudflare answered with a challenge (Bot Fight Mode)
	EdgeError  bool   // Cloudflare could not reach the tunnel (530, 502)
	ServerSays string
}

var hostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// CheckHostname refuses anything but a lowercase DNS name with a dot.
func CheckHostname(h string) (string, error) {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if net.ParseIP(h) != nil || !hostRe.MatchString(h) || len(h) > 253 {
		return "", fmt.Errorf("%q is not a hostname like zm.example.com", h)
	}
	return h, nil
}

// TunnelName is a new tunnel's name for a hostname: potluck-<hostname>, dots
// as dashes. An existing tunnel keeps its name (state.json), and setup
// reuses one named TunnelNameOld (zm-<hostname>) as well.
func TunnelName(host string) string { return "potluck-" + strings.ReplaceAll(host, ".", "-") }

// TunnelNameOld is the name tunnels made before the rename's step 2 have.
func TunnelNameOld(host string) string { return "zm-" + strings.ReplaceAll(host, ".", "-") }

// edgeHost is the Host cloudflared sends for st (State.EdgeHost).
func edgeHost(st State) string {
	if st.EdgeHost != "" {
		return st.EdgeHost
	}
	return tunnel.EdgeHost(st.EdgeSecret)
}

// newEdgeSecret gives st an edge secret if it has none, with the new Host.
func newEdgeSecret(st *State) {
	if st.EdgeSecret != "" {
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	st.EdgeSecret = hex.EncodeToString(b)
	st.EdgeHost = tunnel.EdgeHostNew(st.EdgeSecret)
}

func (s *Setup) dir() string               { return Dir(s.ZmHome) }
func (s *Setup) cfHome() string            { return filepath.Join(s.dir(), "home") }
func (s *Setup) cert() string              { return filepath.Join(s.cfHome(), ".cloudflared", "cert.pem") }
func (s *Setup) creds() string             { return filepath.Join(s.dir(), "credentials.json") }
func (s *Setup) config() string            { return filepath.Join(s.dir(), "config.yml") }
func (s *Setup) statePath() string         { return filepath.Join(s.dir(), "state.json") }
func (s *Setup) logPath() string           { return filepath.Join(s.dir(), "cloudflared.log") }
func (s *Setup) origin() []string          { return []string{"tunnel", "--origincert", s.cert()} }
func (s *Setup) printf(f string, a ...any) { fmt.Fprintf(s.Out, f, a...) }

// LoadState reads state.json (a zero State when there is none).
func LoadState(zmHome string) (State, error) {
	var st State
	b, err := os.ReadFile(filepath.Join(Dir(zmHome), "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

func (s *Setup) save(st *State) error {
	st.Updated = time.Now().UTC()
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeFile(s.statePath(), b)
}

// Label is a new service's label: one per home, as tracker homes do. A
// setup keeps the label its state names (an older one's dev.zm.cloudflared.*).
func Label(zmHome string) string {
	h := fnv32(zmHome)
	return fmt.Sprintf("dev.potluck.cloudflared.%08x", h)
}

// LabelOld is the label services set up before the rename's step 2 have.
func LabelOld(zmHome string) string {
	return fmt.Sprintf("dev.zm.cloudflared.%08x", fnv32(zmHome))
}

// label is st's service label: the one on record, else a new one.
func (s *Setup) label(st State) string {
	if st.Label != "" {
		return st.Label
	}
	return Label(s.ZmHome)
}

func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// Run is zm tunnel setup: idempotent, and it undoes what this run made when
// a step before the check fails.
func (s *Setup) Run(ctx context.Context) (err error) {
	host, err := CheckHostname(s.Hostname)
	if err != nil {
		return err
	}
	if err := s.checkPreviewDomain(); err != nil {
		return err
	}
	if s.Listen == "" {
		s.Listen = DefaultListen
	}
	if err := os.MkdirAll(s.dir(), 0o700); err != nil {
		return err
	}
	st, err := LoadState(s.ZmHome)
	if err != nil {
		return err
	}
	if st.Hostname != "" && st.Hostname != host {
		return fmt.Errorf("this home is already set up for %s; run %s first", st.Hostname, config.Command("tunnel remove"))
	}
	var names []string // the tunnel to reuse: the one on record, else either default name
	switch {
	case s.Name != "":
		names = []string{s.Name}
	case st.Name != "":
		s.Name, names = st.Name, []string{st.Name}
	default:
		s.Name, names = TunnelName(host), []string{TunnelName(host), TunnelNameOld(host)}
	}
	st.Hostname, st.Name, st.Listen, st.Label = host, s.Name, s.Listen, s.label(st)
	if s.PreviewDomain != "" {
		st.PreviewDomain = s.PreviewDomain
	}
	before := st
	var undo []func()
	defer func() {
		if err == nil {
			return
		}
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		if before.TunnelID == "" && st.TunnelID == "" {
			_ = os.Remove(s.statePath())
		} else {
			_ = s.save(&st)
		}
	}()

	// 1. Log in (once per home): cloudflared prints a URL; open it on any
	// device, pick the zone, and it writes the certificate here.
	if _, err := os.Stat(s.cert()); err != nil {
		s.printf(config.Program+": log in to Cloudflare and pick the zone for %s (open this URL on any device; waiting):\n", host)
		if err := os.MkdirAll(s.cfHome(), 0o700); err != nil {
			return err
		}
		if _, err := s.CF.Run(ctx, s.env(), loginPrinter{s.Out}, "tunnel", "login"); err != nil {
			return fmt.Errorf("cloudflared login: %w", err)
		}
		if _, err := os.Stat(s.cert()); err != nil {
			return fmt.Errorf("cloudflared login wrote no certificate at %s", s.cert())
		}
		_ = os.Chmod(s.cert(), 0o600)
		s.printf(config.Program+": logged in (certificate %s, 0600)\n", s.cert())
	}

	// 2. The tunnel: the one with this name, or a new one.
	var id string
	for _, n := range names {
		if id, err = s.findTunnel(ctx, n); err != nil {
			return err
		}
		if id != "" {
			s.Name, st.Name = n, n
			break
		}
	}
	switch {
	case id != "":
		st.TunnelID = id
		if _, err := os.Stat(s.creds()); err != nil {
			if _, err := s.CF.Run(ctx, s.env(), io.Discard, append(s.origin(), "token", "--cred-file", s.creds(), id)...); err != nil {
				return fmt.Errorf("fetching the credentials of tunnel %s: %w", s.Name, err)
			}
		}
		s.printf(config.Program+": tunnel %s (%s) exists; reusing it\n", s.Name, id)
	default:
		out, err := s.CF.Run(ctx, s.env(), io.Discard, append(s.origin(), "create", "--credentials-file", s.creds(), "--output", "json", s.Name)...)
		if err != nil {
			return fmt.Errorf("creating tunnel %s: %w", s.Name, err)
		}
		var t struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(lastJSON(out), &t) != nil || t.ID == "" {
			// Never quote the output: it holds the tunnel's token.
			return fmt.Errorf("creating tunnel %s: no id in cloudflared's %d bytes of output", s.Name, len(out))
		}
		st.TunnelID, st.Created = t.ID, true
		createdID := t.ID
		undo = append(undo, func() {
			if st.Routed && !before.Routed {
				// A DNS record now points at it: deleting the tunnel would
				// leave the name broken, so it stays for a re-run.
				return
			}
			_, _ = s.CF.Run(context.WithoutCancel(ctx), s.env(), io.Discard, append(s.origin(), "delete", "-f", createdID)...)
			_ = os.Remove(s.creds())
			st.TunnelID, st.Created = "", false
		})
		s.printf(config.Program+": created tunnel %s (%s)\n", s.Name, t.ID)
	}
	_ = os.Chmod(s.creds(), 0o600)
	if err := s.save(&st); err != nil {
		return err
	}

	// 3. DNS: <hostname> -> the tunnel (a CNAME the login may create).
	if out, err := s.CF.Run(ctx, s.env(), io.Discard, append(s.origin(), "route", "dns", st.TunnelID, host)...); err != nil {
		if strings.Contains(strings.ToLower(string(out)+err.Error()), "already exists") {
			return fmt.Errorf("%s already has a DNS record that is not this tunnel's; remove it in the Cloudflare dashboard (DNS > Records) or pick another hostname", host)
		}
		return fmt.Errorf("routing %s to tunnel %s: %w", host, s.Name, err)
	}
	st.Routed = true
	if err := s.save(&st); err != nil {
		return err
	}
	s.printf(config.Program+": %s routes to tunnel %s\n", host, s.Name)

	if err := s.routePreview(ctx, st); err != nil {
		return err
	}

	// 4. cloudflared's config, with the edge secret as the Host it sends.
	newEdgeSecret(&st)
	prevCfg, hadCfg := os.ReadFile(s.config())
	if err := writeFile(s.config(), []byte(s.cloudflaredConfig(st))); err != nil {
		return err
	}
	undo = append(undo, func() {
		if hadCfg == nil {
			_ = writeFile(s.config(), prevCfg)
		} else {
			_ = os.Remove(s.config())
		}
	})

	// 5. zm's listener and config (live: no restart).
	was, _ := s.Zm.Tunnel(ctx)
	if _, err := s.Zm.TunnelUp(ctx, api.TunnelUpRequest{Listen: s.Listen, URL: "https://" + host, PreviewDomain: st.PreviewDomain, EdgeSecret: st.EdgeSecret}); err != nil {
		return fmt.Errorf("starting %s's tunnel listener: %w", config.Program, err)
	}
	undo = append(undo, func() {
		if was.URL == "" {
			_, _ = s.Zm.TunnelDown(context.WithoutCancel(ctx))
		}
	})
	s.printf(config.Program+": listening on %s for cloudflared; invites name https://%s\n", s.Listen, host)

	// 6. cloudflared kept running by the service manager, as this user.
	u := s.unit(st)
	hadUnit := fileExists(s.Sup.Path(u))
	if err := s.Sup.Install(ctx, u); err != nil {
		return fmt.Errorf("installing the %s service: %w", s.Sup.Name(), err)
	}
	if !hadUnit {
		undo = append(undo, func() { _ = s.Sup.Remove(context.WithoutCancel(ctx), u) })
	} else if err := s.Sup.Restart(ctx, u); err != nil {
		// Installing a unit that runs already leaves it on the config it
		// started with: a rewritten ingress (runners.listen) takes a restart.
		return fmt.Errorf("restarting the %s service: %w", s.Sup.Name(), err)
	}
	s.printf(config.Program+": cloudflared runs as a %s service (%s), as %s\n", s.Sup.Name(), s.Sup.Path(u), currentUser())
	if s.Linger != nil {
		if on, err := s.Linger(); err == nil && !on {
			s.printf(config.Program+": note: this user's services stop when you log out; to keep the tunnel up on a server run: loginctl enable-linger %s\n", currentUser())
		}
	}
	if err := s.save(&st); err != nil {
		return err
	}

	// 7. Check from outside. A failure here leaves the setup in place: the
	// cause is outside this machine (DNS, the zone), and zm tunnel status
	// looks again.
	undo = nil
	return s.Verify(ctx, host)
}

// QuickWait bounds waiting for cloudflared to print its trycloudflare name.
var QuickWait = 60 * time.Second

var quickURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// RunQuick is the rehearsal (zm tunnel setup --quick): the same service,
// listener and check, on a quick tunnel's temporary trycloudflare name. It
// needs no login and makes nothing in any account; invites made with it
// stop working when the name changes (every cloudflared start).
func (s *Setup) RunQuick(ctx context.Context) (err error) {
	if s.Listen == "" {
		s.Listen = DefaultListen
	}
	if err := os.MkdirAll(s.dir(), 0o700); err != nil {
		return err
	}
	st, err := LoadState(s.ZmHome)
	if err != nil {
		return err
	}
	if st.Hostname != "" && !st.Quick {
		return fmt.Errorf("this home is set up for %s; run %s first", st.Hostname, config.Command("tunnel remove"))
	}
	newEdgeSecret(&st)
	st.Quick, st.Listen, st.Label = true, s.Listen, s.label(st)
	// The edge secret goes in a 0600 config, never on argv or into the
	// service's unit file.
	if err := writeFile(s.config(), []byte(fmt.Sprintf("# Written by zm tunnel setup --quick (SPEC 17.11); zm rewrites it.\nurl: http://%s\nhttp-host-header: %s\nno-autoupdate: true\n",
		s.Listen, edgeHost(st)))); err != nil {
		return err
	}
	u := Unit{Label: st.Label, Args: []string{s.CFPath, "--no-autoupdate", "tunnel", "--config", s.config()},
		Dir: s.dir(), Log: s.logPath(), Env: map[string]string{"HOME": s.cfHome()}}
	_ = os.Remove(s.logPath()) // the name is read from this start's log
	var undo []func()
	defer func() {
		if err != nil {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		}
	}()
	if err := s.Sup.Install(ctx, u); err != nil {
		return fmt.Errorf("installing the %s service: %w", s.Sup.Name(), err)
	}
	undo = append(undo, func() { _ = s.Sup.Remove(context.WithoutCancel(ctx), u) })
	var host string
	for deadline := time.Now().Add(QuickWait); host == "" && time.Now().Before(deadline); {
		if b, err := os.ReadFile(s.logPath()); err == nil {
			host = strings.TrimPrefix(quickURL.FindString(string(b)), "https://")
		}
		if host == "" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	if host == "" {
		return fmt.Errorf("cloudflared printed no trycloudflare name within %s (see %s)", QuickWait, s.logPath())
	}
	if _, err := s.Zm.TunnelUp(ctx, api.TunnelUpRequest{Listen: s.Listen, URL: "https://" + host, PreviewDomain: st.PreviewDomain, EdgeSecret: st.EdgeSecret}); err != nil {
		return fmt.Errorf("starting %s's tunnel listener: %w", config.Program, err)
	}
	undo = append(undo, func() { _, _ = s.Zm.TunnelDown(context.WithoutCancel(ctx)) })
	st.Hostname = host
	if err := s.save(&st); err != nil {
		return err
	}
	s.printf(config.Program+": quick tunnel https://%s (temporary: the name changes when cloudflared restarts)\n", host)
	undo = nil
	return s.Verify(ctx, host)
}

// Verify waits for https://host/v0/health to answer zm's signed 401 from this
// home, and for cloudflared to be seen carrying the edge secret.
func (s *Setup) Verify(ctx context.Context, host string) error {
	probe := s.Probe
	if probe == nil {
		probe = Probe
	}
	wait := s.VerifyFor
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	zs, err := s.Zm.Tunnel(ctx)
	if err != nil {
		return err
	}
	url := "https://" + host + "/v0/health"
	deadline := time.Now().Add(wait)
	var last Answer
	var lastErr error
	for {
		a, err := probe(ctx, url, zs.Fingerprint)
		last, lastErr = a, err
		switch {
		case err == nil && a.Challenge:
			return fmt.Errorf("Cloudflare answers %s with a challenge, which %s's clients cannot pass. %s", url, config.Program, botFightSteps(host))
		case err == nil && a.OtherHome:
			return fmt.Errorf("%s answers as another %s home, or with a signature this home's key (%s) did not make: the hostname points elsewhere", host, config.Program, zs.Fingerprint)
		case err == nil && a.Status == 401 && a.Signed:
			if st, err := s.Zm.Tunnel(ctx); err == nil && st.EdgeSecret && st.EdgeSeen == nil {
				break // through the edge, but not with the secret yet: wait
			}
			s.printf("%s: https://%s answers %s's signed 401 from this home; the edge secret arrives\n", config.Program, host, config.Program)
			s.printf("%s: done. Invite someone with: %s\n", config.Program, config.Command("invite", "<project> --as <name> --role member --via tunnel"))
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	why := fmt.Sprintf("status %d", last.Status)
	if lastErr != nil {
		why = lastErr.Error()
	} else if last.EdgeError {
		why = fmt.Sprintf("Cloudflare could not reach the tunnel (%d): is the %s service running? see %s", last.Status, s.Sup.Name(), s.logPath())
	}
	return fmt.Errorf("https://%s did not answer as this %s home within %s (%s); the setup stays in place: %s checks again", host, config.Program, wait, why, config.Command("tunnel status"))
}

func botFightSteps(host string) string {
	zone := host
	if i := strings.Index(host, "."); i >= 0 {
		zone = host[i+1:]
	}
	return "In the Cloudflare dashboard, open the zone " + zone + ", Security > Bots, and turn Bot Fight Mode off (on the Free plan it cannot be skipped for one hostname). Then run " + config.Command("tunnel status") + "."
}

// env is every cloudflared call's: HOME is the setup's own, so cloudflared
// neither reads nor writes ~/.cloudflared (a config there would change what
// it does).
func (s *Setup) env() []string { return []string{"HOME=" + s.cfHome()} }

func (s *Setup) findTunnel(ctx context.Context, name string) (string, error) {
	out, err := s.CF.Run(ctx, s.env(), io.Discard, append(s.origin(), "list", "--name", name, "--output", "json")...)
	if err != nil {
		return "", fmt.Errorf("listing tunnels: %w", err)
	}
	var ts []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		DeletedAt string `json:"deleted_at"`
	}
	if b := lastJSON(out); len(b) > 0 {
		if err := json.Unmarshal(b, &ts); err != nil {
			return "", fmt.Errorf("listing tunnels: %v", err)
		}
	}
	for _, t := range ts {
		if t.Name == name && (t.DeletedAt == "" || strings.HasPrefix(t.DeletedAt, "0001-")) {
			return t.ID, nil
		}
	}
	return "", nil
}

// Ingress rewrites cloudflared's config for the tunnel on record with
// s.Routes and restarts the service when it changed (cloudflared reads it
// only at start), for a new runners.listen (zm runners setup). It answers
// false, changing nothing, when no named tunnel is set up or nothing
// changed.
func (s *Setup) Ingress(ctx context.Context) (bool, error) {
	st, err := LoadState(s.ZmHome)
	if err != nil || st.Hostname == "" || st.TunnelID == "" || st.Quick {
		return false, err
	}
	if s.Listen = st.Listen; s.Listen == "" {
		s.Listen = DefaultListen
	}
	want := s.cloudflaredConfig(st)
	if have, err := os.ReadFile(s.config()); err == nil && string(have) == want {
		return false, nil
	}
	if s.CFPath == "" {
		return false, fmt.Errorf("no cloudflared to restart (run %s again)", config.Command("tunnel setup", st.Hostname))
	}
	if err := writeFile(s.config(), []byte(want)); err != nil {
		return false, err
	}
	if err := s.Sup.Restart(ctx, s.unit(st)); err != nil {
		return false, fmt.Errorf("restarting the %s service: %w", s.Sup.Name(), err)
	}
	return true, nil
}

// Route is one extra ingress rule: Path (a regex cloudflared matches against
// the request path) on the tunnel's hostname goes to Listen (a loopback
// host:port) instead of zm's door.
type Route struct{ Path, Listen string }

func (s *Setup) cloudflaredConfig(st State) string {
	var extra strings.Builder
	for _, r := range s.Routes {
		fmt.Fprintf(&extra, "  - hostname: %s\n    path: %s\n    service: http://%s\n", st.Hostname, yamlQuote(r.Path), r.Listen)
	}
	return fmt.Sprintf(`# Written by zm tunnel setup (SPEC 17.11); zm rewrites it.
tunnel: %s
credentials-file: %s
no-autoupdate: true
ingress:
%s  - hostname: %s
    service: http://%s
    originRequest:
      httpHostHeader: %s
%s  - service: http_status:404
`, st.TunnelID, s.creds(), extra.String(), st.Hostname, s.Listen, edgeHost(st), s.previewIngress(st))
}

// yamlQuote single-quotes s for YAML (a regex has backslashes and colons).
func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (s *Setup) unit(st State) Unit {
	return Unit{Label: st.Label, Args: []string{s.CFPath, "--no-autoupdate", "tunnel", "--config", s.config(), "run"},
		Dir: s.dir(), Log: s.logPath(), Env: map[string]string{"HOME": s.cfHome()}}
}

// Status is zm tunnel status: what setup made, zm's listener, the service,
// and one look from outside.
func (s *Setup) Status(ctx context.Context) (State, api.TunnelStatus, *Answer, error) {
	st, err := LoadState(s.ZmHome)
	if err != nil {
		return st, api.TunnelStatus{}, nil, err
	}
	zs, err := s.Zm.Tunnel(ctx)
	if err != nil || st.Hostname == "" {
		return st, zs, nil, err
	}
	probe := s.Probe
	if probe == nil {
		probe = Probe
	}
	a, err := probe(ctx, "https://"+st.Hostname+"/v0/health", zs.Fingerprint)
	if err != nil {
		return st, zs, &Answer{ServerSays: err.Error()}, nil
	}
	return st, zs, &a, nil
}

// Remove is zm tunnel remove: zm's listener off, the service gone, and the
// tunnel deleted unless keep. The DNS record cannot be deleted with the
// login certificate: it prints the dashboard step.
func (s *Setup) Remove(ctx context.Context, keepTunnel bool) error {
	st, err := LoadState(s.ZmHome)
	if err != nil {
		return err
	}
	var errs []error
	if _, err := s.Zm.TunnelDown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stopping %s's listener: %w", config.Program, err))
	}
	if st.Label == "" {
		// None on record: a state from before labels were kept, so the old
		// name (a setup by this build always records its label).
		st.Label = LabelOld(s.ZmHome)
	}
	if err := s.Sup.Remove(ctx, s.unit(st)); err != nil {
		errs = append(errs, fmt.Errorf("removing the %s service: %w", s.Sup.Name(), err))
	}
	_ = os.Remove(s.config())
	switch {
	case st.TunnelID == "":
	case keepTunnel || !st.Created:
		// A tunnel this machine reused was made elsewhere: it is not ours to
		// delete, and neither is its DNS record.
		_ = os.Remove(s.creds())
		s.printf(config.Program+": kept tunnel %s (%s) and its DNS record in Cloudflare; this machine no longer runs it\n", st.Name, st.TunnelID)
		keepTunnel = true
	default:
		if _, err := s.CF.Run(ctx, s.env(), io.Discard, append(s.origin(), "delete", "-f", st.TunnelID)...); err != nil {
			errs = append(errs, fmt.Errorf("deleting tunnel %s: %w", st.Name, err))
		} else {
			_ = os.Remove(s.creds())
			s.printf(config.Program+": deleted tunnel %s\n", st.Name)
		}
		if st.Routed {
			s.printf(config.Program+": one step is yours: in the Cloudflare dashboard, DNS > Records, delete the CNAME %s (-> %s.cfargotunnel.com); cloudflared cannot delete DNS records\n", st.Hostname, st.TunnelID)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if keepTunnel && st.Created {
		st.Label = ""
		return s.save(&st) // a later setup reuses the tunnel this machine made
	}
	_ = os.Remove(s.statePath())
	if fileExists(s.cert()) {
		s.printf(config.Program+": removed (the Cloudflare login stays in %s; delete it to log out)\n", s.cfHome())
	} else {
		s.printf("%s: removed\n", config.Program)
	}
	return nil
}

// loginPrinter passes cloudflared's login output through, with the URL on
// a line of its own so it is easy to copy from a headless box.
type loginPrinter struct{ w io.Writer }

var loginURL = regexp.MustCompile(`https://dash\.cloudflare\.com/argotunnel\S*`)

func (p loginPrinter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if u := loginURL.FindString(line); u != "" {
			fmt.Fprintf(p.w, "\n    %s\n\n", u)
			continue
		}
		if strings.TrimSpace(line) != "" {
			fmt.Fprintf(p.w, "  cloudflared: %s\n", line)
		}
	}
	return len(b), nil
}

// lastJSON is the JSON value that ends cloudflared's output (it may log first,
// and its logs may hold brackets): the first '[' or '{' from which the rest is
// one valid value. The value itself holds nested brackets.
func lastJSON(b []byte) []byte {
	s := bytes.TrimSpace(b)
	for i, c := range s {
		if (c == '[' || c == '{') && json.Valid(s[i:]) {
			return s[i:]
		}
	}
	return nil
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return fmt.Sprint(os.Getuid())
}

// IsRoot reports whether this process runs as root (setup refuses unless
// asked: its service would then run as root too).
func IsRoot() bool { return runtime.GOOS != "windows" && os.Geteuid() == 0 }
