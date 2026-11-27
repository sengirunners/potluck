// Package e2bgateway is potluck-e2b (built under its old name potluck-e2b too,
// from cmd/potluck-e2b): it lets the unmodified E2B SDK (Python and TypeScript `e2b`)
// run on boxes (SPEC 20 kind box). It is a socket client of this machine's daemon (potluck-e2b: the
// potluck daemon's; zm-e2b: zm's), like `potluck sandbox`:
// it adds no daemon routes. It answers the E2B control plane (create, get,
// list, kill, set timeout) with the run-target routes, and itself answers
// the envd routes behind `commands.run` and `files.read`/`files.write` with
// box exec: no envd runs in the box.
//
//	potluck-e2b [--addr localhost:49982] [--domain e2b.localhost] [--keys FILE] [--on|--size|--os ...]
//	potluck-e2b keygen [--keys FILE] [--token-file FILE]
//
// A client changes only its environment:
//
//	E2B_DOMAIN=e2b.localhost:49982 E2B_API_KEY=<a key from the keys file>
//	SSL_CERT_FILE=<state>/ca-bundle.pem (Python on Linux) or NODE_EXTRA_CA_CERTS=<state>/ca.pem (Node)
//
// The SDK calls https://api.<E2B_DOMAIN> and https://49983-<id>.<domain>; RFC
// 6761 sends every *.localhost name to loopback (macOS, systemd hosts), and
// the gateway signs a certificate per name from its own CA, name-constrained
// to the domain (no wildcard certificate). Owned by sandboxes (zm-eakh.12.15).
package e2bgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/attach"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

// Main runs the program on argv (without the program name) and returns its
// exit code.
func Main(argv []string) int {
	if len(argv) > 0 && argv[0] == "keygen" {
		return keygen(argv[1:])
	}
	return serve(argv)
}

// stateDir is the gateway's state directory and the client config of the
// daemon it serves from, chosen by the one-daemon rule (package attach): on
// a machine whose Potluck daemon is another program's, and where this
// program has no home, that daemon (also one that is restarting: attach.
// Starting). The gateway makes no home of its own then, and writes nothing
// under another program's home: its state goes where --state says, and
// without it the gateway refuses.
func stateDir(flagged string) (string, *config.Config, error) {
	v := attach.Vars{Home: config.HomeEnv()}
	ctx := context.Background()
	h, err := attach.Resolve(ctx, v)
	if err != nil {
		return "", nil, err
	}
	if flagged != "" {
		return flagged, h.Cfg, nil
	}
	if h.Owner == "" {
		if o := attach.Starting(ctx, v, h); o != nil {
			h.Owner, h.Cfg = o.Program, &config.Config{Home: o.Home}
		}
	}
	if h.Owner != "" {
		return "", nil, fmt.Errorf("%s's home is %s, where this machine's Potluck daemon runs or starts, and the gateway writes nothing under another program's home: "+
			"say where its CA, token secret and keys go with --state <dir>", h.Owner, attach.Tilde(h.Cfg.Home))
	}
	return h.Cfg.Path("e2b"), h.Cfg, nil
}

func serve(args []string) int {
	fs := flag.NewFlagSet("potluck-e2b", flag.ContinueOnError)
	addr := fs.String("addr", "localhost:49982", "listen address (localhost listens on 127.0.0.1 and ::1)")
	domain := fs.String("domain", "e2b.localhost", "the E2B domain's host part; clients use api.<domain> and <port>-<id>.<domain>")
	advertise := fs.String("advertise", "", "E2B_DOMAIN as clients set it (default <domain>:<port of --addr>)")
	state := fs.String("state", "", "state directory: CA, token secret, keys (default $"+config.HomeEnv()+"/e2b; with another program's daemon, required)")
	keys := fs.String("keys", "", "API keys file, mode 0600 (default <state>/keys; potluck-e2b keygen writes it)")
	tlsCert := fs.String("tls-cert", "", "serve this certificate instead of the gateway's own CA (with --tls-key)")
	tlsKey := fs.String("tls-key", "", "the key for --tls-cert")
	tlsOnly := fs.Bool("tls-only", false, "refuse plain HTTP (required for a non-loopback --addr)")
	project := fs.String("project", "", "project (default $ZM_PROJECT, then the repo you are in)")
	var want computeclient.BoxWant
	fs.StringVar(&want.On, "on", "", "this box run target (default: placement, as "+config.PotluckCommand("sandbox create")+")")
	fs.StringVar(&want.Size, "size", "", "a size the box target states")
	fs.StringVar(&want.OS, "os", "", "linux or macos")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir, cfg, err := stateDir(*state)
	if err != nil {
		return fail(err)
	}
	if *keys == "" {
		*keys = filepath.Join(dir, "keys")
	}
	base := client.New(cfg)
	// Never a session address: a session-bound lease renews while its session
	// lives (SPEC 20 "Leases"), and an E2B sandbox must end at its timeout.
	base.As = ""
	if *project != "" {
		base.Project, base.ProjectFrom = *project, "flag"
	}
	host, port, err := net.SplitHostPort(*addr)
	if err != nil {
		return fail(fmt.Errorf("--addr: %w", err))
	}
	if err := checkAddr(host, *tlsOnly); err != nil {
		return fail(err)
	}
	if *advertise == "" {
		*advertise = net.JoinHostPort(*domain, port)
	}
	g, err := newGateway(gatewayConfig{Domain: *domain, Advertise: *advertise, StateDir: dir, KeysFile: *keys, Want: want}, base)
	if err != nil {
		return fail(err)
	}
	tc, err := g.tlsConfig(*tlsCert, *tlsKey)
	if err != nil {
		return fail(err)
	}
	hosts := []string{host}
	if host == "localhost" {
		hosts = []string{"127.0.0.1", "::1"}
	}
	srv := &http.Server{Handler: g, TLSConfig: tc, ReadHeaderTimeout: 30 * time.Second}
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	srv.Protocols = &p
	errc := make(chan error, len(hosts))
	served := 0
	for _, h := range hosts {
		l, err := net.Listen("tcp", net.JoinHostPort(h, port))
		if err != nil {
			if served == 0 && h == hosts[len(hosts)-1] {
				return fail(err)
			}
			fmt.Fprintln(os.Stderr, "potluck-e2b:", err)
			continue
		}
		served++
		go func() { errc <- srv.Serve(newSniffListener(l, tc, !*tlsOnly)) }()
	}
	if served == 0 {
		return fail(errors.New("no listener"))
	}
	also := " (plain HTTP on the same port)"
	if *tlsOnly {
		also = ""
	}
	fmt.Fprintf(os.Stderr, "potluck-e2b: serving https://api.%s%s\n", *advertise, also)
	fmt.Fprintf(os.Stderr, "  export E2B_DOMAIN=%s E2B_API_KEY=<a key in %s>\n", *advertise, *keys)
	if *tlsCert == "" {
		fmt.Fprintf(os.Stderr, "  export SSL_CERT_FILE=%s NODE_EXTRA_CA_CERTS=%s\n", filepath.Join(dir, "ca-bundle.pem"), filepath.Join(dir, "ca.pem"))
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case err := <-errc:
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	return 0
}

// keygen appends a fresh API key to the keys file (created 0600) and prints
// it. With --token-file the key acts as that zm token's identity; without,
// as the gateway's own (its socket caller).
func keygen(args []string) int {
	fs := flag.NewFlagSet("potluck-e2b keygen", flag.ContinueOnError)
	state := fs.String("state", "", "state directory (default $"+config.HomeEnv()+"/e2b; with another program's daemon, required)")
	keys := fs.String("keys", "", "API keys file (default <state>/keys)")
	tokenFile := fs.String("token-file", "", "a 0600 file holding the token this key acts as")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir, _, err := stateDir(*state)
	if err != nil {
		return fail(err)
	}
	if *keys == "" {
		*keys = filepath.Join(dir, "keys")
	}
	line, key, err := newKeyLine(*tokenFile)
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(*keys), 0o700); err != nil {
		return fail(err)
	}
	f, err := os.OpenFile(*keys, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Mode().Perm() != 0o600 {
		return fail(fmt.Errorf("%s must be mode 0600", *keys))
	}
	if _, err := f.WriteString(line); err != nil {
		return fail(err)
	}
	fmt.Println(key)
	return 0
}

func newKeyLine(tokenFile string) (line, key string, err error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	key = "e2b_" + hex.EncodeToString(b)
	line = key
	if tokenFile != "" {
		tok, err := readSecretFile(tokenFile)
		if err != nil {
			return "", "", err
		}
		line += " " + tok
	}
	return line + "\n", key, nil
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "potluck-e2b:", strings.TrimSpace(err.Error()))
	return 1
}
