// Package githubfake is a fake GitHub on httptest for the owners brick's
// tests: GET /user by token, and the OAuth device flow. No test talks to the
// live GitHub.
package githubfake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// GitHub is the fake. Its URL serves both the API and the web (device flow)
// paths.
type GitHub struct {
	*httptest.Server
	ClientID string // the device flow's only accepted client id
	// Interval is the polling interval the device flow asks for, in
	// seconds (default 1; GitHub's is 5). SlowDown polls answer slow_down
	// (the client must add 5 seconds) before the others.
	Interval int
	SlowDown int

	mu       sync.Mutex
	users    map[int64]*User
	tokens   map[string]int64           // token -> user id
	devices  map[string]int64           // device code -> user id that will approve it
	pending  map[string]int             // device code -> polls still answering authorization_pending
	slow     map[string]int             // device code -> polls still answering slow_down
	starts   int                        // device codes asked for
	issued   []string                   // tokens the device flow handed out
	members  map[int64]map[int64]string // org id -> user id -> state (active, pending)
	gone     map[int64]bool             // suspended or deleted: 404 everywhere
	lookups  int
	failMem  bool
	admins   map[int64]map[int64]bool // org id -> user id -> admin // /user/memberships/orgs answers 500
	userHits int
	limited  time.Time // until then, the lookup endpoints answer the rate limit
	refused  int       // calls refused for the rate limit
	next     int64
	seq      int
}

// User is a fake account.
type User struct {
	ID    int64
	Login string
	Type  string // User, Organization, Bot
}

// New starts a fake GitHub, closed when t ends.
func New(t testing.TB) *GitHub {
	g := &GitHub{ClientID: "Iv1.fakeclient", users: map[int64]*User{}, tokens: map[string]int64{},
		devices: map[string]int64{}, pending: map[string]int{}, slow: map[string]int{}, members: map[int64]map[int64]string{},
		gone: map[int64]bool{}, next: 583000}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", g.user)
	mux.HandleFunc("GET /users/{login}", g.byLogin)
	mux.HandleFunc("GET /user/{id}", g.byID)
	mux.HandleFunc("GET /user/memberships/orgs/{org}", g.membership)
	mux.HandleFunc("GET /orgs/{org}/members/{login}", g.orgMember)
	mux.HandleFunc("POST /login/device/code", g.deviceCode)
	mux.HandleFunc("POST /login/oauth/access_token", g.accessToken)
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

// AddUser makes an account (typ "User", "Organization" or "Bot") and
// returns it and a token for it.
func (g *GitHub) AddUser(login, typ string) (User, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	u := &User{ID: g.next, Login: login, Type: typ}
	g.users[u.ID] = u
	return *u, g.tokenLocked(u.ID)
}

// Token mints another token for user id.
func (g *GitHub) Token(id int64) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tokenLocked(id)
}

func (g *GitHub) tokenLocked(id int64) string {
	g.seq++
	tok := "gho_fake" + strconv.Itoa(g.seq) + "x" + strconv.FormatInt(id, 10)
	g.tokens[tok] = id
	return tok
}

// Rename changes a login (GitHub keeps the id).
func (g *GitHub) Rename(id int64, login string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.users[id].Login = login
}

// AddMember makes user a member of org with state (active or pending).
func (g *GitHub) AddMember(org, user int64, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.members[org] == nil {
		g.members[org] = map[int64]string{}
	}
	g.members[org][user] = state
}

// Gone suspends or deletes an account (gone true) or brings it back: a
// gone account answers 404 by id and by login, and its tokens 401.
func (g *GitHub) Gone(id int64, gone bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gone[id] = gone
}

// Lookups counts GET /users/{login} and GET /user/{id} requests.
func (g *GitHub) Lookups() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lookups
}

// Revoke makes token answer 401.
func (g *GitHub) Revoke(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.tokens, token)
}

// Approver sets who approves the next device codes (id 0: the person
// declines; -1: the code expires), and how many polls answer
// authorization_pending first.
func (g *GitHub) Approver(id int64, pendingPolls int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.devices[""] = id
	g.pending[""] = pendingPolls
}

// Starts counts the device codes asked for.
func (g *GitHub) Starts() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.starts
}

// Issued lists the tokens the device flow handed out.
func (g *GitHub) Issued() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.issued...)
}

// UserCalls counts GET /user requests.
func (g *GitHub) UserCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.userHits
}

// RateLimitUntil makes the lookup endpoints (by login, by id, an org's
// members) refuse with GitHub's rate-limit answer (403, no calls remaining,
// X-RateLimit-Reset at t, whole seconds, rounded up) until t.
func (g *GitHub) RateLimitUntil(t time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limited = t
}

// RateLimited counts the calls refused for the rate limit.
func (g *GitHub) RateLimited() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refused
}

// limitedLocked answers the rate limit when one is set; g.mu is held.
func (g *GitHub) limitedLocked(w http.ResponseWriter) bool {
	if !time.Now().Before(g.limited) {
		return false
	}
	g.refused++
	reset := g.limited.Add(time.Second - 1).Unix()
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
	writeJSON(w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded"})
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (g *GitHub) user(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.userHits++
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	id, known := g.tokens[tok]
	u := g.users[id]
	if !ok || !known || u == nil || g.gone[id] {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	writeJSON(w, http.StatusOK, userJSON(u))
}

func userJSON(u *User) map[string]any {
	return map[string]any{"id": u.ID, "login": u.Login, "type": u.Type}
}

func (g *GitHub) byLogin(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limitedLocked(w) {
		return
	}
	g.lookups++
	for _, u := range g.users {
		if strings.EqualFold(u.Login, r.PathValue("login")) && !g.gone[u.ID] {
			writeJSON(w, http.StatusOK, userJSON(u))
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (g *GitHub) byID(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limitedLocked(w) {
		return
	}
	g.lookups++
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if u := g.users[id]; u != nil && !g.gone[id] {
		writeJSON(w, http.StatusOK, userJSON(u))
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (g *GitHub) membership(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failMem {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "Server Error"})
		return
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	id, known := g.tokens[tok]
	if !known || g.gone[id] {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	for _, o := range g.users {
		if strings.EqualFold(o.Login, r.PathValue("org")) && o.Type == "Organization" && g.members[o.ID][id] != "" {
			role := "member"
			if g.admins[o.ID][id] {
				role = "admin"
			}
			writeJSON(w, http.StatusOK, map[string]any{"state": g.members[o.ID][id], "role": role})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

// orgMember is GET /orgs/{org}/members/{login}: a member's token sees
// every member (204, else 404); anyone else is redirected (302).
func (g *GitHub) orgMember(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limitedLocked(w) {
		return
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	viewer, known := g.tokens[tok]
	if tok != "" && !known {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	var org *User
	for _, o := range g.users {
		if strings.EqualFold(o.Login, r.PathValue("org")) && o.Type == "Organization" && !g.gone[o.ID] {
			org = o
		}
	}
	if org == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	if g.members[org.ID][viewer] != "active" {
		w.Header().Set("Location", "/orgs/"+org.Login+"/public_members/"+r.PathValue("login"))
		w.WriteHeader(http.StatusFound)
		return
	}
	for id, u := range g.users {
		if strings.EqualFold(u.Login, r.PathValue("login")) && g.members[org.ID][id] == "active" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

// FailMemberships makes the membership endpoint answer 500 (or not).
func (g *GitHub) FailMemberships(fail bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failMem = fail
}

// MakeAdmin makes user an active admin of org.
func (g *GitHub) MakeAdmin(org, user int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.members[org] == nil {
		g.members[org] = map[int64]string{}
	}
	if g.admins == nil {
		g.admins = map[int64]map[int64]bool{}
	}
	if g.admins[org] == nil {
		g.admins[org] = map[int64]bool{}
	}
	g.members[org][user], g.admins[org][user] = "active", true
}

// RemoveMember takes user out of org.
func (g *GitHub) RemoveMember(org, user int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.members[org], user)
}

func (g *GitHub) deviceCode(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != g.ClientID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not Found"}) // GitHub's answer to an unknown client id
		return
	}
	if r.PostForm.Get("client_secret") != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the device flow sends no secret"})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.starts++
	g.seq++
	code := "dc" + strconv.Itoa(g.seq)
	g.devices[code], g.pending[code] = g.devices[""], g.pending[""]
	writeJSON(w, http.StatusOK, map[string]any{"device_code": code, "user_code": "WDJB-MJHT",
		"verification_uri": g.URL + "/login/device", "expires_in": 900, "interval": max(g.Interval, 1)})
	g.slow[code] = g.SlowDown
}

func (g *GitHub) accessToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != g.ClientID ||
		r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
		writeJSON(w, http.StatusOK, map[string]string{"error": "incorrect_client_credentials"})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	code := r.PostForm.Get("device_code")
	id, ok := g.devices[code]
	switch {
	case !ok || id == 0:
		writeJSON(w, http.StatusOK, map[string]string{"error": "access_denied"})
	case id < 0:
		writeJSON(w, http.StatusOK, map[string]string{"error": "expired_token"})
	case g.slow[code] > 0:
		g.slow[code]--
		writeJSON(w, http.StatusOK, map[string]string{"error": "slow_down", "interval": strconv.Itoa(max(g.Interval, 1) + 5)})
	case g.pending[code] > 0:
		g.pending[code]--
		writeJSON(w, http.StatusOK, map[string]string{"error": "authorization_pending"}) // GitHub answers 200
	default:
		delete(g.devices, code)
		tok := g.tokenLocked(id)
		g.issued = append(g.issued, tok)
		writeJSON(w, http.StatusOK, map[string]string{"access_token": tok, "token_type": "bearer",
			"scope": r.PostForm.Get("scope")})
	}
}
