package server_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	. "github.com/sengirunners/potluck/kit/server"
)

// fakeNet is a tailnet seen from the daemon's node: its peers only.
type fakeNet struct{ peers []core.NetPeer }

func (fakeNet) Dial(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed }
func (fakeNet) Peer(context.Context, string) (core.Remote, error) {
	return core.Remote{}, net.ErrClosed
}
func (n fakeNet) Self(context.Context) (core.NetSelf, error) {
	return core.NetSelf{Running: true, NodeID: "nSELF", DNSName: "potluck-host.example.ts.net", PeerList: n.peers}, nil
}
func (fakeNet) Up(context.Context, string, time.Duration) (api.TailnetStatus, error) {
	return api.TailnetStatus{}, nil
}

const (
	phoneID  = "nPHONE"
	phoneKey = "nodekey:phone"
)

var testPeers = []core.NetPeer{
	{Host: "phone", DNSName: "phone.example.ts.net", NodeID: phoneID, NodeKey: phoneKey, Login: "owner@example.com", OS: "iOS"},
	{Host: "potluck-host", DNSName: "potluck-host.example.ts.net", NodeID: "nSELF", NodeKey: "nodekey:self"},
	{Host: "ci", DNSName: "ci.example.ts.net", NodeID: "nCI", NodeKey: "nodekey:ci", Tagged: true},
	{Host: "zm-other", DNSName: "zm-other.example.ts.net", NodeID: "nZM", NodeKey: "nodekey:zm"},
	{Host: "router", DNSName: "router.example.ts.net", NodeID: "nRT", NodeKey: "nodekey:rt", Relays: true},
	{Host: "guest", DNSName: "guest.other.ts.net", NodeID: "nSH", NodeKey: "nodekey:sh", Shared: true},
	{Host: "nokey", DNSName: "nokey.example.ts.net", NodeID: "nNK"},
	{Host: "twin", DNSName: "twin.example.ts.net", NodeID: "nT1", NodeKey: "nodekey:t1"},
	{Host: "twin", DNSName: "twin.other.ts.net", NodeID: "nT2", NodeKey: "nodekey:t2"},
	// It calls itself "laptop", but the tailnet names it "spoof".
	{Host: "laptop", DNSName: "spoof.example.ts.net", NodeID: "nSP", NodeKey: "nodekey:sp"},
}

// seen records which layer saw which request.
type seen struct {
	mu   sync.Mutex
	hits map[string][]string // layer -> "METHOD path"
	pub  []string            // what the public route saw: "subject" / "token" when present
}

func (s *seen) add(layer string, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits[layer] = append(s.hits[layer], r.Method+" "+r.URL.Path)
}

func (s *seen) take() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hits
	s.hits = map[string][]string{}
	return h
}

type proxyRec struct{ s *seen }

func (p proxyRec) Proxy(_ http.ResponseWriter, r *http.Request, _ http.Handler) bool {
	p.s.add("proxy", r)
	return false
}

// walkModule registers routes with and without a limit.
type walkModule struct{ s *seen }

func (m walkModule) Routes(r *core.Router) {
	r = r.For("walk")
	h := func(w http.ResponseWriter, req *http.Request) {
		m.s.add("handler", req)
		api.WriteJSON(w, http.StatusOK, map[string]string{"limit": core.SubjectOf(req.Context()).Limit()})
	}
	r.HandleFunc("GET /v0/walk/open/{id}", core.Authed, "read", h)
	r.HandleFunc("POST /v0/walk/open/{id}", core.Authed, "write", h)
	r.HandleFunc("GET /v0/walk/closed", core.Authed, "read", h)
	r.HandleFunc("POST /v0/walk/closed", core.Authed, "write", h)
	r.HandleFunc("GET /v0/walk/local", core.Local, "local", h)
	r.HandleFunc("POST /v0/walk/device", core.Device, "device", h)
	r.HandleFunc("GET /v0/walk/public", core.Public, "none", func(w http.ResponseWriter, req *http.Request) {
		m.s.add("handler", req)
		m.s.mu.Lock()
		if core.SubjectOf(req.Context()) != nil {
			m.s.pub = append(m.s.pub, "subject")
		}
		if req.Header.Get(api.HeaderAuthorization) != "" {
			m.s.pub = append(m.s.pub, "token")
		}
		m.s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	r.Admit("answer", "GET /v0/walk/open/{id}", "POST /v0/walk/open/{id}")
}

type limitFixture struct {
	t      *testing.T
	dm     *Daemon
	s      *seen
	socket *client.Client
	secret string
	id     string
}

func newLimitFixture(t *testing.T, cfgFile func(*config.File)) *limitFixture {
	t.Helper()
	cfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "owner", "host"
	if cfgFile != nil {
		cfgFile(&cfg.File)
	}
	s := &seen{hits: map[string][]string{}}
	dm, err := New(context.Background(), cfg, DiscardLog(), func(d *core.Deps) ([]core.Module, error) {
		d.Net = fakeNet{peers: testPeers}
		d.Team = proxyRec{s}
		d.AddRequestGate(func(w http.ResponseWriter, r *http.Request, serve func(http.ResponseWriter, *http.Request)) bool {
			s.add("gate", r)
			return false
		})
		return []core.Module{walkModule{s}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	f := &limitFixture{t: t, dm: dm, s: s, socket: client.WithHandler(dm.SocketHandler())}
	if _, err := f.socket.CreateProject(context.Background(), api.NewProjectRequest{Slug: "proj"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *limitFixture) mint(req api.MintTokenRequest) (api.MintTokenResponse, error) {
	return f.socket.MintToken(context.Background(), req)
}

func (f *limitFixture) mintAnswer() api.MintTokenResponse {
	f.t.Helper()
	out, err := f.mint(api.MintTokenRequest{Limit: "answer", Project: "proj", Node: "phone", TTLSeconds: 3600})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

var atPhone = core.Remote{Login: "owner@example.com", Node: "phone.example.ts.net", NodeID: phoneID, NodeKey: phoneKey}

// call sends method path with secret over a network listener that
// identified rm (nil: a listener without WhoIs).
func (f *limitFixture) call(method, path, secret string, rm *core.Remote) *httptest.ResponseRecorder {
	// A deadline ends a streaming route (GET /v0/events) the walk calls.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader("{}"))
	if secret != "" {
		req.Header.Set(api.HeaderAuthorization, "Bearer "+secret)
	}
	req.Header.Set("Content-Type", "application/json")
	if rm != nil {
		req = req.WithContext(core.WithRemote(req.Context(), *rm))
	}
	w := httptest.NewRecorder()
	f.dm.Handler.ServeHTTP(w, req)
	return w
}

func apiErr(t *testing.T, w *httptest.ResponseRecorder) api.Error {
	t.Helper()
	var e struct {
		Error api.Error `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Error
}

func isTokenLimit(t *testing.T, w *httptest.ResponseRecorder) bool {
	e := apiErr(t, w)
	return w.Code == http.StatusForbidden && e.Code == api.CodeForbidden && string(e.Data) == `{"reason":"token-limit"}`
}

// A limited token reaches the routes that admit its limit and nothing
// else: the fallback, unmatched paths and every other route answer 403
// token-limit before any gate, the team proxy or a handler sees the request.
func TestLimitedTokenRouteWalk(t *testing.T) {
	f := newLimitFixture(t, nil)
	out := f.mintAnswer()
	if out.Device == nil || out.Device.Name != "phone.example.ts.net" || out.Device.Login != "owner@example.com" || out.Device.OS != "iOS" ||
		out.Token.Limit != "answer" || out.Token.Device != "phone.example.ts.net" {
		t.Fatalf("mint answer %+v %+v", out, out.Device)
	}
	f.s.take()

	for _, c := range []struct{ method, path string }{
		{"GET", "/v0/walk/open/x"}, {"HEAD", "/v0/walk/open/x"}, {"POST", "/v0/walk/open/x"},
	} {
		w := f.call(c.method, c.path, out.Secret, &atPhone)
		if w.Code != http.StatusOK {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
		h := f.s.take()
		if len(h["gate"]) != 1 || len(h["proxy"]) != 1 || len(h["handler"]) != 1 {
			t.Errorf("%s %s seen %v", c.method, c.path, h)
		}
	}
	w := f.call("GET", "/v0/whoami", out.Secret, &atPhone)
	var who api.Whoami
	if json.Unmarshal(w.Body.Bytes(), &who); w.Code != http.StatusOK || who.Limit != "answer" || who.Via != "token" {
		t.Fatalf("whoami %d %s", w.Code, w.Body)
	}
	f.s.take()

	// Every registered route but the admitted ones and the public ones,
	// plus paths no route has.
	var refused []struct{ method, path string }
	for _, rt := range f.dm.Router.Routes() {
		if rt.Access == core.Public || rt.Pattern == "GET /v0/whoami" || strings.HasPrefix(rt.Pattern, "GET /v0/walk/open/") || strings.HasPrefix(rt.Pattern, "POST /v0/walk/open/") {
			continue
		}
		method, path, _ := strings.Cut(rt.Pattern, " ")
		refused = append(refused, struct{ method, path string }{method, fillPattern(path)})
	}
	refused = append(refused,
		struct{ method, path string }{"GET", "/v0/nope"},
		struct{ method, path string }{"GET", "/"},
		struct{ method, path string }{"PUT", "/v0/walk/open/x"},
		struct{ method, path string }{"DELETE", "/v0/whoami"},
		struct{ method, path string }{"GET", "/v0/walk/open/x/more"},
		struct{ method, path string }{"GET", "/v0/walk//open/x"},
	)
	if len(refused) < 10 {
		t.Fatalf("walked only %d routes", len(refused))
	}
	for _, c := range refused {
		w := f.call(c.method, c.path, out.Secret, &atPhone)
		if !isTokenLimit(t, w) {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	if h := f.s.take(); len(h) != 0 {
		t.Fatalf("refused requests were seen: %v", h)
	}

	// A public route runs, with no subject and no token.
	for _, p := range []string{"/v0/walk/public", "/v0/health"} {
		if w := f.call("GET", p, out.Secret, &atPhone); w.Code >= 300 {
			t.Errorf("public %s: %d %s", p, w.Code, w.Body)
		}
	}
	// What health tells an unauthenticated caller, not its owner.
	var h api.Health
	if w := f.call("GET", "/v0/health", out.Secret, &atPhone); json.Unmarshal(w.Body.Bytes(), &h) != nil || h.PID != 0 {
		t.Fatalf("health for a limited token: %s", w.Body)
	}
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if len(f.s.pub) != 0 {
		t.Fatalf("the public route saw %v", f.s.pub)
	}
}

func fillPattern(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if strings.HasPrefix(s, "{") {
			parts[i] = "x"
		}
	}
	return strings.Join(parts, "/")
}

// The same token from anywhere but its pinned device is an unknown token,
// and so is a revoked or expired one, or one whose identity no longer owns
// the project.
func TestLimitedTokenOnlyFromItsDevice(t *testing.T) {
	f := newLimitFixture(t, nil)
	out := f.mintAnswer()
	unknown := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		e := apiErr(t, w)
		if w.Code != http.StatusUnauthorized || e.Code != api.CodeUnauthenticated || e.Message != "invalid or expired token" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	other, rekeyed, self := atPhone, atPhone, atPhone
	other.NodeID = "nOTHER"
	rekeyed.NodeKey = "nodekey:new"
	self.Self = true
	tunnel := core.Remote{Via: core.ViaTunnel, NodeID: phoneID, NodeKey: phoneKey}
	for name, rm := range map[string]*core.Remote{"no WhoIs": nil, "another node": &other, "a re-keyed node": &rekeyed, "this host": &self, "the tunnel": &tunnel} {
		unknown(name, f.call("GET", "/v0/walk/open/x", out.Secret, rm))
	}
	// The socket.
	req := httptest.NewRequest("GET", "/v0/walk/open/x", nil)
	req.Header.Set(api.HeaderAuthorization, "Bearer "+out.Secret)
	w := httptest.NewRecorder()
	f.dm.SocketHandler().ServeHTTP(w, req)
	unknown("the socket", w)
	if h := f.s.take(); len(h["handler"]) != 0 {
		t.Fatalf("a refused caller reached a handler: %v", h)
	}

	revoked := f.mintAnswer()
	if _, err := f.socket.RevokeToken(context.Background(), revoked.Token.ID); err != nil {
		t.Fatal(err)
	}
	unknown("revoked", f.call("GET", "/v0/walk/open/x", revoked.Secret, &atPhone))
	expired := f.mintAnswer()
	if _, err := f.dm.Deps.Store.Registry().Exec(`UPDATE tokens SET expires = 1 WHERE id = ?`, expired.Token.ID); err != nil {
		t.Fatal(err)
	}
	unknown("expired", f.call("GET", "/v0/walk/open/x", expired.Secret, &atPhone))

	if w := f.call("GET", "/v0/walk/open/x", out.Secret, &atPhone); w.Code != http.StatusOK {
		t.Fatalf("pinned device: %d %s", w.Code, w.Body)
	}
	// The identity's grant lowered below owner.
	if _, err := f.dm.Deps.Store.Registry().Exec(`UPDATE grants SET relation = 'member' WHERE area = ''`); err != nil {
		t.Fatal(err)
	}
	unknown("grant lowered", f.call("GET", "/v0/walk/open/x", out.Secret, &atPhone))
}

func TestLimitedMintRules(t *testing.T) {
	f := newLimitFixture(t, func(c *config.File) { c.LimitedTokenMaxTTLSeconds = 7200 })
	base := api.MintTokenRequest{Limit: "answer", Project: "proj", Node: "phone", TTLSeconds: 3600}
	for name, c := range map[string]struct {
		mut  func(*api.MintTokenRequest)
		code string
	}{
		"this host's node":           {func(r *api.MintTokenRequest) { r.Node = "potluck-host" }, api.CodeBadRequest},
		"a tagged node":              {func(r *api.MintTokenRequest) { r.Node = "ci" }, api.CodeBadRequest},
		"a daemon's node":            {func(r *api.MintTokenRequest) { r.Node = "zm-other" }, api.CodeBadRequest},
		"a relay":                    {func(r *api.MintTokenRequest) { r.Node = "router" }, api.CodeBadRequest},
		"a shared-in node":           {func(r *api.MintTokenRequest) { r.Node = "guest.other.ts.net" }, api.CodeBadRequest},
		"a node with no key":         {func(r *api.MintTokenRequest) { r.Node = "nokey" }, api.CodeBadRequest},
		"two nodes answer the name":  {func(r *api.MintTokenRequest) { r.Node = "twin" }, api.CodeConflict},
		"a self-reported host name":  {func(r *api.MintTokenRequest) { r.Node = "laptop" }, api.CodeNotFound},
		"no such node":               {func(r *api.MintTokenRequest) { r.Node = "tablet" }, api.CodeNotFound},
		"no node":                    {func(r *api.MintTokenRequest) { r.Node = "" }, api.CodeBadRequest},
		"a node without a limit":     {func(r *api.MintTokenRequest) { r.Limit = "" }, api.CodeBadRequest},
		"a limit no route admits":    {func(r *api.MintTokenRequest) { r.Limit = "other" }, api.CodeBadRequest},
		"the any-limit marker":       {func(r *api.MintTokenRequest) { r.Limit = core.AnyLimit }, api.CodeBadRequest},
		"a session":                  {func(*api.MintTokenRequest) {}, api.CodeForbidden},
		"another identity":           {func(r *api.MintTokenRequest) { r.As = "someone" }, api.CodeBadRequest},
		"an area":                    {func(r *api.MintTokenRequest) { r.Area = "a" }, api.CodeBadRequest},
		"no ttl":                     {func(r *api.MintTokenRequest) { r.TTLSeconds = 0 }, api.CodeBadRequest},
		"a ttl over the set ceiling": {func(r *api.MintTokenRequest) { r.TTLSeconds = 7201 }, api.CodeBadRequest},
		"no project":                 {func(r *api.MintTokenRequest) { r.Project = "" }, api.CodeBadRequest},
		"a member role":              {func(r *api.MintTokenRequest) { r.Role = "member" }, api.CodeBadRequest},
	} {
		req := base
		c.mut(&req)
		mint := f.mint
		if name == "a session" {
			sc := client.WithHandler(f.dm.SocketHandler())
			sc.As = "host/abcd1234"
			mint = func(r api.MintTokenRequest) (api.MintTokenResponse, error) {
				return sc.MintToken(context.Background(), r)
			}
		}
		if _, err := mint(req); !api.IsCode(err, c.code) {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	// By the full name and by the first label alike.
	for _, n := range []string{"phone", "phone.example.ts.net", "PHONE.example.ts.net."} {
		req := base
		req.Node = n
		if _, err := f.mint(req); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	// A network caller never reaches the mint (a local route).
	out := f.mintAnswer()
	if w := f.call("POST", "/v0/tokens", out.Secret, &atPhone); !isTokenLimit(t, w) {
		t.Fatalf("mint with a limited token: %d %s", w.Code, w.Body)
	}
}

// Admit is checked at start: a pattern no route has, a malformed limit and
// a route that never takes a token each stop the daemon from starting.
func TestAdmitCheckedAtStart(t *testing.T) {
	for name, admit := range map[string]func(*core.Router){
		"unregistered":  func(r *core.Router) { r.Admit("answer", "GET /v0/nothing") },
		"bad name":      func(r *core.Router) { r.Admit("an_swer", "GET /v0/projects") },
		"a local route": func(r *core.Router) { r.Admit("answer", "POST /v0/tokens") },
		"public":        func(r *core.Router) { r.Admit("answer", "GET /v0/health") },
	} {
		cfg, err := config.LoadFrom(shortHome(t))
		if err != nil {
			t.Fatal(err)
		}
		dm, err := New(context.Background(), cfg, DiscardLog(), func(*core.Deps) ([]core.Module, error) {
			return []core.Module{admitModule(admit)}, nil
		})
		if err == nil {
			dm.Close()
			t.Errorf("%s: started", name)
		}
	}
}

type admitModule func(*core.Router)

func (m admitModule) Routes(r *core.Router) { m(r) }

// An unlimited token on the network and the socket's owner see every route
// exactly as before: no route answers token-limit to them, and whoami
// carries no limit field.
func TestUnlimitedCallersNeverLimited(t *testing.T) {
	f := newLimitFixture(t, nil)
	plain, err := f.mint(api.MintTokenRequest{As: "helper", Project: "proj", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range f.dm.Router.Routes() {
		method, path, _ := strings.Cut(rt.Pattern, " ")
		if path == "/v0/daemon/stop" {
			continue
		}
		path = fillPattern(path)
		if w := f.call(method, path, plain.Secret, &atPhone); isTokenLimit(t, w) {
			t.Errorf("token %s %s: token-limit", method, path)
		}
	}
	w := f.call("GET", "/v0/whoami", plain.Secret, &atPhone)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"limit"`) {
		t.Fatalf("whoami %d %s", w.Code, w.Body)
	}
}
