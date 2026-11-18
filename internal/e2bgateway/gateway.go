package e2bgateway

// The E2B control plane (the REST routes the pinned SDKs call) over the
// run-target routes. A sandbox is a box; its id derives from the handle.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/protocol"
)

const (
	// envdVersion is the envd version the gateway answers as: new enough
	// that the SDKs send no default-user header and may upload as
	// octet-stream (0.5.7), older than file metadata (0.6.2).
	envdVersion = "0.5.7"
	envdPort    = "49983"
	// defaultTimeoutS is E2B's default sandbox timeout.
	defaultTimeoutS = 300
	// requestPrefix marks the acquires this gateway made; the key's tag
	// follows it (keyTag), so a box found by scanning goes to its key only.
	requestPrefix = "e2b-"
)

type gatewayConfig struct {
	Domain    string // host part: names are api.<Domain> and <port>-<id>.<Domain>
	Advertise string // E2B_DOMAIN as clients set it (Domain[:port]), answered as "domain"
	StateDir  string
	KeysFile  string
	Want      computeclient.BoxWant
}

type gateway struct {
	cfg    gatewayConfig
	keys   []*apiKey
	secret []byte
	now    func() time.Time

	mu        sync.Mutex
	sandboxes map[string]*sandbox // by sandbox id
}

// sandbox is what the gateway knows of a box beyond its handle. Metadata
// and envs live only here: a gateway restart forgets them (the box stays).
type sandbox struct {
	id, handle, target, hid string
	template                string
	key                     *apiKey
	metadata, envs          map[string]string
}

func newGateway(cfg gatewayConfig, base *client.Client) (*gateway, error) {
	if err := validDomain(cfg.Domain); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	keys, err := loadKeys(cfg.KeysFile, base)
	if err != nil {
		return nil, err
	}
	secret, err := loadSecret(filepath.Join(cfg.StateDir, "token-secret"))
	if err != nil {
		return nil, err
	}
	return &gateway{cfg: cfg, keys: keys, secret: secret, now: time.Now, sandboxes: map[string]*sandbox{}}, nil
}

// sandboxIDPrefix starts a sandbox id ("zm" before the rename's step 2).
const sandboxIDPrefix = "pl"

// sandboxID derives a sandbox's id from its box handle: a DNS label, so
// 49983-<id>.<domain> is a valid name.
func sandboxID(handle string) string {
	sum := sha256.Sum256([]byte(handle))
	return sandboxIDPrefix + hex.EncodeToString(sum[:11])
}

// e2bError is the control plane's error body.
type e2bError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, format string, a ...any) {
	writeJSON(w, status, e2bError{Code: status, Message: fmt.Sprintf(format, a...)})
}

// writeZm answers a daemon error with its status and message.
func writeZm(w http.ResponseWriter, err error) {
	status, msg := http.StatusInternalServerError, err.Error()
	var ae *api.Error
	if errors.As(err, &ae) {
		status, msg = ae.Status(), ae.Message
	}
	writeErr(w, status, "potluck: %s", msg)
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("E2b-Sandbox-Id") != "" || strings.HasPrefix(r.Host, envdPort+"-") {
		g.serveEnvd(w, r)
		return
	}
	k := match(g.keys, r.Header.Get("X-API-Key"))
	if k == nil {
		writeErr(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/v2")
	parts := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case p == "/sandboxes" && r.Method == http.MethodPost:
		g.create(w, r, k)
	case p == "/sandboxes" && r.Method == http.MethodGet:
		g.list(w, r, k)
	case len(parts) == 2 && parts[0] == "sandboxes":
		switch r.Method {
		case http.MethodGet:
			g.get(w, r, k, parts[1])
		case http.MethodDelete:
			g.kill(w, r, k, parts[1])
		default:
			writeErr(w, http.StatusMethodNotAllowed, "%s %s is not served", r.Method, r.URL.Path)
		}
	case len(parts) == 3 && parts[0] == "sandboxes" && r.Method == http.MethodPost && parts[2] == "timeout":
		g.setTimeout(w, r, k, parts[1], "timeout")
	case len(parts) == 3 && parts[0] == "sandboxes" && r.Method == http.MethodPost && parts[2] == "refreshes":
		g.setTimeout(w, r, k, parts[1], "duration")
	case len(parts) == 3 && parts[0] == "sandboxes" && r.Method == http.MethodPost && parts[2] == "connect":
		g.connect(w, r, k, parts[1])
	default:
		writeErr(w, http.StatusNotImplemented, "potluck-e2b does not serve %s %s (v1: create, get, list, kill, set timeout, connect)", r.Method, r.URL.Path)
	}
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// createBody is NewSandbox (v1) and NewSandboxV2: what zm can honour, and
// what it must refuse rather than quietly drop.
type createBody struct {
	TemplateID          string            `json:"templateID"`
	Timeout             *int              `json:"timeout"`
	Metadata            map[string]string `json:"metadata"`
	EnvVars             map[string]string `json:"envVars"`
	AutoPause           bool              `json:"autoPause"`
	AllowInternetAccess *bool             `json:"allow_internet_access"`
	Network             json.RawMessage   `json:"network"`
	VolumeMounts        json.RawMessage   `json:"volumeMounts"`
	Mcp                 json.RawMessage   `json:"mcp"`
}

func empty(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null" || s == "{}" || s == "[]"
}

func (g *gateway) create(w http.ResponseWriter, r *http.Request, k *apiKey) {
	var b createBody
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body: %v", err)
		return
	}
	switch {
	case b.AutoPause:
		writeErr(w, http.StatusBadRequest, "potluck boxes do not pause (autoPause)")
		return
	case b.AllowInternetAccess != nil && !*b.AllowInternetAccess, !empty(b.Network):
		writeErr(w, http.StatusBadRequest, "potluck-e2b v1 cannot enforce network rules; refusing rather than ignoring them")
		return
	case !empty(b.VolumeMounts):
		writeErr(w, http.StatusBadRequest, "potluck-e2b v1 has no volumes")
		return
	case !empty(b.Mcp):
		writeErr(w, http.StatusBadRequest, "potluck-e2b v1 has no MCP gateway")
		return
	}
	for name := range b.EnvVars {
		if !protocol.SecretName(name) {
			writeErr(w, http.StatusBadRequest, "envVars name %q is not [A-Z][A-Z0-9_]{0,63} (potluck-e2b sends envs as secrets, one file per name)", name)
			return
		}
	}
	ttl := defaultTimeoutS
	if b.Timeout != nil {
		ttl = *b.Timeout
	}
	if ttl <= 0 {
		writeErr(w, http.StatusBadRequest, "timeout must be positive")
		return
	}
	ctx := r.Context()
	req := computeapi.AcquireRunTargetRequest{TTLS: ttl, RequestID: keyTag(k) + randomID()}
	if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" {
		// Per API key: token-less keys share one zm actor, and the daemon
		// dedups request ids per actor.
		sum := sha256.Sum256([]byte(k.name + "\x00" + key))
		req.RequestID = keyTag(k) + "k" + hex.EncodeToString(sum[:12])
	}
	if b.TemplateID != "" && b.TemplateID != "base" {
		req.Image = b.TemplateID // templates are out of v1: a template names one of the binding's images
	}
	target, h, err := computeclient.AcquireBox(ctx, k.c, g.cfg.Want, req, nil)
	if err != nil {
		writeZm(w, err)
		return
	}
	if h.State != "ready" { // a replayed Idempotency-Key whose box has ended
		writeErr(w, http.StatusConflict, "the sandbox made for this Idempotency-Key is %s; use a new key", h.State)
		return
	}
	t, hid, err := computeclient.SplitBoxHandle(h.Handle)
	if err != nil {
		if i := strings.LastIndex(h.Handle, "/"); i >= 0 { // never leave a box nobody can name
			_, _ = computeclient.ReleaseRunTarget(ctx, k.c, target, h.Handle[i+1:])
		}
		writeZm(w, err)
		return
	}
	s := &sandbox{id: sandboxID(h.Handle), handle: h.Handle, target: t, hid: hid, template: b.TemplateID,
		key: k, metadata: b.Metadata, envs: b.EnvVars}
	if s.template == "" {
		s.template = "base"
	}
	g.mu.Lock()
	if prev := g.sandboxes[s.id]; prev != nil && prev.key == k { // an idempotent repeat keeps what the first said
		s = prev
	}
	g.sandboxes[s.id] = s
	g.mu.Unlock()
	writeJSON(w, http.StatusCreated, g.sandboxView(s))
}

// keyTag is the request id prefix of the boxes made through k.
func keyTag(k *apiKey) string { return requestPrefix + k.name + "-" }

// madeBy says whether h was acquired through key k.
func madeBy(h computeapi.RunTargetHandle, k *apiKey) bool {
	return strings.Contains(h.RequestID, keyTag(k))
}

func randomID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sandboxView is E2B's Sandbox (create and connect answers).
func (g *gateway) sandboxView(s *sandbox) map[string]any {
	return map[string]any{
		"templateID":         s.template,
		"sandboxID":          s.id,
		"clientID":           "potluck",
		"envdVersion":        envdVersion,
		"envdAccessToken":    accessToken(g.secret, s.key, s.id),
		"trafficAccessToken": nil,
		"domain":             g.cfg.Advertise,
	}
}

// lookup finds a live sandbox the key may see: from memory, else by
// scanning the boxes key's identity sees (after a gateway restart).
func (g *gateway) lookup(ctx context.Context, k *apiKey, id string) (*sandbox, computeapi.RunTargetHandle, error) {
	g.mu.Lock()
	s := g.sandboxes[id]
	g.mu.Unlock()
	if s == nil {
		hs, _, err := computeclient.Sandboxes(ctx, k.c, computeclient.HandleFilter{Live: true})
		if err != nil {
			return nil, computeapi.RunTargetHandle{}, err
		}
		for _, h := range hs {
			if sandboxID(h.Handle) == id && madeBy(h, k) {
				s = g.remember(k, h)
				break
			}
		}
	}
	if s == nil || s.key != k {
		return nil, computeapi.RunTargetHandle{}, api.NotFound("sandbox", id)
	}
	h, err := computeclient.RunTargetStatus(ctx, k.c, s.target, s.hid)
	if err != nil {
		return nil, h, err
	}
	if h.State != "ready" {
		g.forget(id)
		return nil, h, api.NotFound("sandbox", id)
	}
	return s, h, nil
}

// remember records a box found by scanning (no metadata or envs survive a
// gateway restart); only a box made through k.
func (g *gateway) remember(k *apiKey, h computeapi.RunTargetHandle) *sandbox {
	t, hid, err := computeclient.SplitBoxHandle(h.Handle)
	if err != nil || !madeBy(h, k) {
		return nil
	}
	id := sandboxID(h.Handle)
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.sandboxes[id]; s != nil {
		return s
	}
	s := &sandbox{id: id, handle: h.Handle, target: t, hid: hid, template: "base", key: k}
	g.sandboxes[id] = s
	return s
}

func (g *gateway) forget(id string) {
	g.mu.Lock()
	delete(g.sandboxes, id)
	g.mu.Unlock()
}

// sizeOf is a binding's described size, for cpuCount and memoryMB.
func sizeOf(ctx context.Context, c *client.Client, target string) (cpu, memMB, diskMB int) {
	v, err := computeclient.DescribeRunTarget(ctx, c, target)
	if err != nil || v.Describe == nil || v.Describe.Size == nil {
		return 0, 0, 0
	}
	sz := v.Describe.Size
	return sz.CPU, sz.MemoryMB, sz.DiskGB * 1024
}

// detail is E2B's SandboxDetail and ListedSandbox.
func (g *gateway) detail(s *sandbox, h computeapi.RunTargetHandle, cpu, mem, disk int) map[string]any {
	started := h.Acquired
	if started == "" {
		started = h.Created
	}
	md := s.metadata
	if md == nil {
		md = map[string]string{}
	}
	return map[string]any{
		"templateID":      s.template,
		"sandboxID":       s.id,
		"clientID":        "potluck",
		"startedAt":       started,
		"endAt":           h.ExpiresAt,
		"envdVersion":     envdVersion,
		"envdAccessToken": accessToken(g.secret, s.key, s.id),
		"domain":          g.cfg.Advertise,
		"cpuCount":        cpu,
		"memoryMB":        mem,
		"diskSizeMB":      disk,
		"metadata":        md,
		"state":           "running",
	}
}

func (g *gateway) get(w http.ResponseWriter, r *http.Request, k *apiKey, id string) {
	s, h, err := g.lookup(r.Context(), k, id)
	if err != nil {
		writeZm(w, err)
		return
	}
	cpu, mem, disk := sizeOf(r.Context(), k.c, s.target)
	writeJSON(w, http.StatusOK, g.detail(s, h, cpu, mem, disk))
}

// list answers GET /sandboxes and /v2/sandboxes: the key's live sandboxes,
// newest first, filtered by ?metadata (k=v&...) and ?state, cut at ?limit.
// Everything fits one page: no X-Next-Token.
func (g *gateway) list(w http.ResponseWriter, r *http.Request, k *apiKey) {
	q := r.URL.Query()
	if st := q["state"]; len(st) > 0 {
		running := false
		for _, v := range st {
			for _, p := range strings.Split(v, ",") {
				running = running || p == "running"
			}
		}
		if !running {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
	}
	var want url.Values
	if m := q.Get("metadata"); m != "" {
		var err error
		if want, err = url.ParseQuery(m); err != nil {
			writeErr(w, http.StatusBadRequest, "bad metadata filter")
			return
		}
	}
	limit := 0
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "bad limit")
			return
		}
		limit = n
	}
	ctx := r.Context()
	hs, skipped, err := computeclient.Sandboxes(ctx, k.c, computeclient.HandleFilter{Live: true})
	if err != nil {
		writeZm(w, err)
		return
	}
	live := map[string]bool{}
	for _, h := range hs {
		if h.State == "ready" && madeBy(h, k) {
			live[sandboxID(h.Handle)] = true
		}
	}
	if len(skipped) == 0 { // every binding answered: drop what has ended
		g.mu.Lock()
		for id, s := range g.sandboxes {
			if s.key == k && !live[id] {
				delete(g.sandboxes, id)
			}
		}
		g.mu.Unlock()
	}
	sizes := map[string][3]int{}
	out := []any{}
	for _, h := range hs {
		if !live[sandboxID(h.Handle)] {
			continue
		}
		s := g.remember(k, h)
		if s == nil || s.key != k || !metaMatch(s.metadata, want) {
			continue
		}
		sz, ok := sizes[s.target]
		if !ok {
			c, m, d := sizeOf(ctx, k.c, s.target)
			sz = [3]int{c, m, d}
			sizes[s.target] = sz
		}
		out = append(out, g.detail(s, h, sz[0], sz[1], sz[2]))
		if limit > 0 && len(out) == limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func metaMatch(have map[string]string, want url.Values) bool {
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := have[k]; !ok || v != want.Get(k) {
			return false
		}
	}
	return true
}

func (g *gateway) kill(w http.ResponseWriter, r *http.Request, k *apiKey, id string) {
	s, _, err := g.lookup(r.Context(), k, id)
	if err != nil {
		writeZm(w, err)
		return
	}
	if _, err := computeclient.ReleaseRunTarget(r.Context(), k.c, s.target, s.hid); err != nil {
		writeZm(w, err)
		return
	}
	g.forget(id)
	w.WriteHeader(http.StatusNoContent)
}

// setTimeout is POST .../timeout ({"timeout"}) and .../refreshes
// ({"duration"}): the box's deadline becomes now + that many seconds, a
// renew within the binding's ttl_max_s (past it, the daemon's bad_request).
func (g *gateway) setTimeout(w http.ResponseWriter, r *http.Request, k *apiKey, id, field string) {
	var b map[string]int
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body: %v", err)
		return
	}
	secs, ok := b[field]
	if field == "duration" && !ok {
		secs, ok = 60, true // E2B's refresh default
	}
	if !ok || secs <= 0 {
		writeErr(w, http.StatusBadRequest, "%s must be a positive number of seconds", field)
		return
	}
	s, _, err := g.lookup(r.Context(), k, id)
	if err != nil {
		writeZm(w, err)
		return
	}
	if _, err := computeclient.RenewRunTarget(r.Context(), k.c, s.target, s.hid, secs); err != nil {
		writeZm(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// connect is POST /sandboxes/{id}/connect and /v2/...: a running sandbox's
// Sandbox, its deadline raised to now + timeout if that is later.
func (g *gateway) connect(w http.ResponseWriter, r *http.Request, k *apiKey, id string) {
	var b struct {
		Timeout int `json:"timeout"`
	}
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body: %v", err)
		return
	}
	s, h, err := g.lookup(r.Context(), k, id)
	if err != nil {
		writeZm(w, err)
		return
	}
	if b.Timeout > 0 {
		exp, perr := time.Parse(time.RFC3339Nano, h.ExpiresAt)
		if perr != nil || g.now().Add(time.Duration(b.Timeout)*time.Second).After(exp) {
			if _, err := computeclient.RenewRunTarget(r.Context(), k.c, s.target, s.hid, b.Timeout); err != nil {
				writeZm(w, err)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, g.sandboxView(s))
}
