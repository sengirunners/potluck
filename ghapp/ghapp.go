// Package ghapp is a minimal GitHub App client for a runner broker. It mints
// App JWTs and cached installation tokens, mints and revokes uncached
// one-repository tokens for a holder (token.go), reads installations, repos, runs and
// jobs (and lists them with conditional requests, lists.go), registers and removes self-hosted runners (JIT config), checks that a
// user can see an installation, drives the App manifest flow, and verifies
// webhook signatures. Every base URL comes from config, so GitHub Enterprise
// works. Standard library only.
package ghapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxBody      = 1 << 20
	tokenSlack   = 5 * time.Minute
	defaultGroup = 1
)

// Config configures a Client.
type Config struct {
	AppID         int64
	PrivateKeyPEM []byte // PKCS#1 "RSA PRIVATE KEY" (as GitHub issues it) or PKCS#8
	APIURL        string // e.g. https://api.github.com
	HTTP          *http.Client
	Now           func() time.Time
	// Sleep waits between retries (MintToken, RevokeToken, RepoInstallation);
	// nil is a timer that ends early with ctx. Tests pass a fake.
	Sleep func(context.Context, time.Duration) error
}

// Client talks to GitHub as one App. Safe for concurrent use.
type Client struct {
	appID  int64
	key    *rsa.PrivateKey
	api    string
	hc     *http.Client
	strict *http.Client // hc refusing redirects: the token calls (token.go)
	now    func() time.Time
	mu     sync.Mutex
	tokens map[string]*tokEntry // installation|repo|permissions
	// pruneAt is the size of tokens at which a new entry sweeps it again.
	pruneAt int
	rates   map[int64]rate // installation -> its last known API budget

	sleepFn func(context.Context, time.Duration) error
}

type tokEntry struct {
	users int // calls holding the entry, under Client.mu
	mu    sync.Mutex
	tok   string
	exp   time.Time
}

// New parses the key and returns a Client. Errors never echo key material.
func New(c Config) (*Client, error) {
	if c.AppID <= 0 {
		return nil, errors.New("ghapp: AppID must be positive")
	}
	if strings.TrimSpace(c.APIURL) == "" {
		return nil, errors.New("ghapp: APIURL is empty")
	}
	block, _ := pem.Decode(c.PrivateKeyPEM)
	if block == nil {
		return nil, errors.New("ghapp: private key is not PEM")
	}
	key, err := parseKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	cl := &Client{appID: c.AppID, key: key, api: strings.TrimRight(c.APIURL, "/"),
		hc: httpClient(c.HTTP), now: c.Now, tokens: map[string]*tokEntry{}, sleepFn: c.Sleep}
	if cl.now == nil {
		cl.now = time.Now
	}
	strict := *cl.hc
	strict.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cl.strict = &strict
	return cl, nil
}

func parseKey(der []byte) (*rsa.PrivateKey, error) {
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	k8, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("ghapp: private key is neither PKCS#1 nor PKCS#8")
	}
	k, ok := k8.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("ghapp: private key is not an RSA key")
	}
	return k, nil
}

func httpClient(h *http.Client) *http.Client {
	if h != nil {
		return h
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwt returns an App JWT: RS256, iat = now-60s, exp = now+9min, iss = AppID.
func (c *Client) jwt() (string, error) {
	now := c.now()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	cl, _ := json.Marshal(map[string]int64{
		"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": c.appID})
	signing := b64(hdr) + "." + b64(cl)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("ghapp: signing the App JWT failed")
	}
	return signing + "." + b64(sig), nil
}

// call performs one request and decodes a JSON response into out (if non-nil).
// Statuses in want are success; anything else becomes an error.
func call(ctx context.Context, hc *http.Client, method, base, path, bearer string, in, out any, want ...int) error {
	_, err := callH(ctx, hc, method, base, path, bearer, in, out, want...)
	return err
}

// callH is call answering the response's headers too (nil when none came).
func callH(ctx context.Context, hc *http.Client, method, base, path, bearer string, in, out any, want ...int) (http.Header, error) {
	h, _, err := callHdr(ctx, hc, method, base, path, bearer, nil, in, out, want...)
	return h, err
}

// callHdr is callH sending the headers in hdr too and answering the status.
// A 304 in want leaves out as it was: it has no body.
func callHdr(ctx context.Context, hc *http.Client, method, base, path, bearer string, hdr http.Header, in, out any,
	want ...int) (http.Header, int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		return nil, 0, fmt.Errorf("github %s %s: bad request", method, path)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := httpClient(hc).Do(req)
	if err != nil {
		// url.Error carries only the URL, never headers.
		return nil, 0, fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return resp.Header, resp.StatusCode, fmt.Errorf("github %s %s: reading response: %w", method, path, err)
	}
	if len(data) > maxBody {
		return resp.Header, resp.StatusCode, fmt.Errorf("github %s %s: the response is over %d MiB: not read", method, path, maxBody>>20)
	}
	ok := false
	for _, w := range want {
		ok = ok || resp.StatusCode == w
	}
	if !ok {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return resp.Header, resp.StatusCode, &statusError{method, path, resp.StatusCode, msg}
	}
	if out != nil && resp.StatusCode != http.StatusNotModified {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.Header, resp.StatusCode, fmt.Errorf("github %s %s: decoding response: %w", method, path, err)
		}
	}
	return resp.Header, resp.StatusCode, nil
}

// statusError is GitHub answering a status the call did not want.
type statusError struct {
	method, path string
	status       int
	msg          string
}

// NotFound reports whether err is GitHub answering 404: the thing asked
// about is gone (or the token cannot see it).
func NotFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

// NoRepoAccess reports whether err is GitHub refusing the App the
// repository itself: 404 (the repository gone, or the token cannot see it),
// or 422 minting the repository's token (the repository is not in the
// installation, or the installation lacks the permission asked for).
func NoRepoAccess(err error) bool {
	var se *statusError
	return NotFound(err) || errors.As(err, &se) && se.status == http.StatusUnprocessableEntity && strings.Contains(se.path, "/access_tokens")
}

// RunnerBusy reports whether err is GitHub refusing to delete a runner it has
// given a job (DeleteRunner).
func RunnerBusy(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.method == http.MethodDelete && se.status == http.StatusUnprocessableEntity && strings.Contains(se.path, "/actions/runners/")
}

func (e *statusError) Error() string {
	return fmt.Sprintf("github %s %s: %d %s", e.method, e.path, e.status, e.msg)
}

func (c *Client) asApp(ctx context.Context, method, path string, in, out any, want ...int) error {
	j, err := c.jwt()
	if err != nil {
		return err
	}
	return call(ctx, c.hc, method, c.api, path, j, in, out, want...)
}

func (c *Client) asInstall(ctx context.Context, inst int64, repo string, p Perms, method, path string, in, out any, want ...int) error {
	t, err := c.InstallationToken(ctx, inst, repo, p)
	if err != nil {
		return err
	}
	hdr, err := callH(ctx, c.hc, method, c.api, path, t, in, out, want...)
	c.noteRate(inst, hdr)
	return err
}

// rate is an installation's API budget as GitHub's last answer said it.
type rate struct {
	remaining, limit int
	reset            time.Time
}

// rateFloor: at or below this share of an installation's hourly budget (or
// this many calls) RateLow says so, and the broker's waiting serves make no
// call until the reset; ends, takeovers and the sweep still do.
const (
	rateFloorShare = 10
	rateFloorCalls = 100
)

// noteRate records an installation's budget from a response's headers
// (X-RateLimit-*; Retry-After on a secondary limit).
func (c *Client) noteRate(inst int64, h http.Header) {
	if h == nil {
		return
	}
	rem, err1 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	lim, _ := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	reset, err2 := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	r := rate{remaining: rem, limit: lim, reset: time.Unix(reset, 0)}
	if ra, err := strconv.Atoi(h.Get("Retry-After")); err == nil && ra > 0 {
		r = rate{remaining: 0, limit: max(lim, 1), reset: c.now().Add(time.Duration(min(ra, 3600)) * time.Second)}
	} else if err1 != nil || err2 != nil {
		return
	}
	c.mu.Lock()
	if c.rates == nil {
		c.rates = map[int64]rate{}
	}
	c.rates[inst] = r
	c.mu.Unlock()
}

// holdRate counts inst's budget as spent for d (GitHub refused for a rate
// limit and said nothing of when it resets), unless a later reset is known.
func (c *Client) holdRate(inst int64, d time.Duration) {
	until := c.now().Add(d)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rates == nil {
		c.rates = map[int64]rate{}
	}
	if r, ok := c.rates[inst]; ok && r.remaining == 0 && r.reset.After(until) {
		return
	}
	c.rates[inst] = rate{remaining: 0, limit: 1, reset: until}
}

// rateLimited reports whether err is GitHub refusing for a rate limit: a
// 429, or a 403 with Retry-After, no budget left, or a message that says so
// (a secondary limit).
func rateLimited(err error, hdr http.Header) bool {
	var se *statusError
	if !errors.As(err, &se) {
		return false
	}
	return se.status == http.StatusTooManyRequests || (se.status == http.StatusForbidden &&
		(strings.Contains(strings.ToLower(se.msg), "rate limit") ||
			hdr != nil && (hdr.Get("Retry-After") != "" || hdr.Get("X-RateLimit-Remaining") == "0")))
}

// RateLow reports whether inst's budget is below the floor, and until when.
func (c *Client) RateLow(inst int64) (time.Time, bool) {
	c.mu.Lock()
	r, ok := c.rates[inst]
	c.mu.Unlock()
	if !ok || !c.now().Before(r.reset) {
		return time.Time{}, false
	}
	floor := min(rateFloorCalls, r.limit*rateFloorShare/100)
	return r.reset, r.remaining <= floor
}

// Perms is a token's permission set: GitHub permission name to "read" or "write".
type Perms map[string]string

// readPerms is what reading a repo, its runs and its jobs needs.
var readPerms = Perms{"actions": "read", "metadata": "read"}

// key is the permissions in a fixed order, for the token cache.
func (p Perms) key() string {
	ks := make([]string, 0, len(p))
	for k, v := range p {
		ks = append(ks, k+"="+v)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func (p Perms) validate() error {
	if len(p) == 0 {
		return errors.New("ghapp: a token needs at least one permission")
	}
	for k, v := range p {
		if !validName(k) || (v != "read" && v != "write") {
			return fmt.Errorf("ghapp: invalid permission %s=%s", k, v)
		}
	}
	return nil
}

// ErrNoInstallation is a token GitHub refused because the App has no such
// installation (404: uninstalled, or never installed): every repository of
// it fails alike.
var ErrNoInstallation = errors.New("the App has no such installation")

// ErrInstallationRefused is a token GitHub refused for the installation as a
// whole, not for a repository: 401 (the App's key is not accepted) or 403
// (the installation is suspended), other than a rate limit. Every repository
// of the installation fails alike.
var ErrInstallationRefused = errors.New("GitHub refuses the App's token for this installation")

// WholeInstallation reports whether err is a token refusal that every
// repository of the installation shares: ErrNoInstallation or
// ErrInstallationRefused.
func WholeInstallation(err error) bool {
	return errors.Is(err, ErrNoInstallation) || errors.Is(err, ErrInstallationRefused)
}

// Answered reports whether err is GitHub's own answer (a status the call did
// not want), as opposed to a failure to reach it.
func Answered(err error) bool {
	var se *statusError
	return errors.As(err, &se)
}

// refusedToken is a 401 or a 403 that is not a rate limit.
func refusedToken(err error, hdr http.Header) bool {
	var se *statusError
	return errors.As(err, &se) && (se.status == http.StatusUnauthorized || se.status == http.StatusForbidden) && !rateLimited(err, hdr)
}

// InstallationToken returns a cached installation token that names one
// repository (repo, owner/name, of the installation's account) and only the
// permissions p, refreshing it when fewer than 5 minutes remain. A leaked token
// so reaches one repository with what the call needs, never the whole
// installation with all the App's permissions. RevokeCached ends them.
func (c *Client) InstallationToken(ctx context.Context, installation int64, repo string, p Perms) (string, error) {
	if err := validRepo(repo); err != nil {
		return "", err
	}
	if err := p.validate(); err != nil {
		return "", err
	}
	return c.cachedToken(ctx, installation, repo, p)
}

// cachedToken is InstallationToken's cache and mint for repo (owner/name),
// or for no repository when repo is "" (AccountToken). Callers validate.
func (c *Client) cachedToken(ctx context.Context, installation int64, repo string, p Perms) (string, error) {
	key := strconv.FormatInt(installation, 10) + "|" + strings.ToLower(repo) + "|" + p.key()
	c.mu.Lock()
	e := c.tokens[key]
	if e == nil {
		c.pruneTokens()
		e = &tokEntry{}
		c.tokens[key] = e
	}
	e.users++ // pinned: pruneTokens leaves an entry a call holds, so the map keeps every live token
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		e.users--
		c.mu.Unlock()
	}()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tok != "" && c.now().Add(tokenSlack).Before(e.exp) {
		return e.tok, nil
	}
	var r struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	in := map[string]any{"permissions": p}
	if repo != "" {
		_, name, _ := strings.Cut(repo, "/")
		in["repositories"] = []string{name}
	}
	path := "/app/installations/" + strconv.FormatInt(installation, 10) + "/access_tokens"
	j, err := c.jwt()
	if err != nil {
		return "", err
	}
	if hdr, err := callH(ctx, c.hc, http.MethodPost, c.api, path, j, in, &r, http.StatusCreated); err != nil {
		switch {
		case NotFound(err):
			return "", fmt.Errorf("%w: %w", ErrNoInstallation, err)
		case refusedToken(err, hdr):
			return "", fmt.Errorf("%w: %w", ErrInstallationRefused, err)
		}
		return "", err
	}
	if r.Token == "" {
		return "", fmt.Errorf("github POST %s: response has no token", path)
	}
	e.tok, e.exp = r.Token, r.ExpiresAt
	return e.tok, nil
}

// minPruneAt is how many cached entries the token map holds before a new one
// sweeps out the ones with nothing in them.
const minPruneAt = 64

// pruneTokens drops the entries holding no live token (expired, or never
// minted) that no call holds, so a repository that is no longer asked about
// does not stay in the map. Callers hold c.mu.
func (c *Client) pruneTokens() {
	if len(c.tokens) < max(c.pruneAt, minPruneAt) {
		return
	}
	now := c.now()
	for k, e := range c.tokens {
		if e.users > 0 || !e.mu.TryLock() {
			continue // a call holds it (it may be minting its token)
		}
		if e.tok == "" || !now.Before(e.exp) {
			delete(c.tokens, k)
		}
		e.mu.Unlock()
	}
	c.pruneAt = 2 * len(c.tokens)
}

// Installation is an App installation.
type Installation struct {
	ID          int64
	Account     string
	AccountType string // "Organization" or "User"
}

// Installation reads one installation (App JWT).
func (c *Client) Installation(ctx context.Context, id int64) (Installation, error) {
	var r struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	if err := c.asApp(ctx, http.MethodGet, "/app/installations/"+strconv.FormatInt(id, 10), nil, &r, 200); err != nil {
		return Installation{}, err
	}
	return Installation{ID: r.ID, Account: r.Account.Login, AccountType: r.Account.Type}, nil
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

func validName(s string) bool { return nameRE.MatchString(s) && !strings.Contains(s, "..") && s != "." }

func validRepo(full string) error {
	o, r, ok := strings.Cut(full, "/")
	if !ok || !validName(o) || !validName(r) {
		return fmt.Errorf("ghapp: invalid repository name %q", full)
	}
	return nil
}

// Repo is repository metadata.
type Repo struct {
	ID         int64 // GitHub's id: kept across a rename, never reused for another repository
	FullName   string
	Private    bool
	Visibility string
	Fork       bool
	// DefaultBranch is the branch GitHub names default ("" when it did not say).
	DefaultBranch string
}

// Public is the one rule for whether a repository counts as public: whether
// people beyond those its owner chose can read it, see its runs and propose
// code to it. Public and internal repositories count (an internal one is
// open to its whole enterprise, and GitHub answers private: true for it); so
// does an answer that is not private, or a visibility this version does not
// know. Only a private repository whose visibility is "private" does not,
// or one that is private with no visibility said: GitHub has said
// visibility on every repository since internal repositories exist, and
// the runner broker decides again on GitHub's full answer (Client.Repo)
// before any runner is minted, so an answer without it (a run's short
// repository object, an old server) never lets a runner start alone. The
// runner box's own check of a job's event payload (vmhost jitLaunch) is
// stricter: it has nothing to read again, so there private needs
// visibility "private" said.
func Public(private bool, visibility string) bool {
	return !private || visibility != "" && visibility != "private"
}

// Audience names a repository as Public judges it: kind is what to call it
// ("private" for one that does not count as public; "internal"; "public",
// or the visibility GitHub named when this version does not know it) and
// who is everyone beyond those its owner chose who can see it ("" when
// private).
func Audience(private bool, visibility string) (kind, who string) {
	switch {
	case !Public(private, visibility):
		return "private", ""
	case visibility == "internal":
		return "internal", "everyone in its enterprise"
	case visibility == "" || visibility == "private" || visibility == "public":
		return "public", "anyone"
	}
	return visibility, "anyone"
}

// Article is "a" or "an" for kind (Audience), as English spells it.
func Article(kind string) string {
	if kind != "" && strings.ContainsRune("aeiou", rune(kind[0])) {
		return "an"
	}
	return "a"
}

// Public reports whether r counts as public (the package's Public).
func (r Repo) Public() bool { return Public(r.Private, r.Visibility) }

// Repo reads GET /repos/{owner}/{repo}.
func (c *Client) Repo(ctx context.Context, installation int64, fullName string) (Repo, error) {
	if err := validRepo(fullName); err != nil {
		return Repo{}, err
	}
	var r struct {
		ID         int64  `json:"id"`
		FullName   string `json:"full_name"`
		Private    bool   `json:"private"`
		Visibility string `json:"visibility"`
		Fork       bool   `json:"fork"`
		Default    string `json:"default_branch"`
	}
	if err := c.asInstall(ctx, installation, fullName, readPerms, http.MethodGet, "/repos/"+fullName, nil, &r, 200); err != nil {
		return Repo{}, err
	}
	if r.ID == 0 {
		return Repo{}, fmt.Errorf("github: %s: the answer names no repository id", fullName)
	}
	return Repo{r.ID, r.FullName, r.Private, r.Visibility, r.Fork, r.Default}, nil
}

// Run is a workflow run.
type Run struct {
	ID     int64
	Event  string
	Status string // "completed" once every job of its latest attempt ended
	// Conclusion is how a completed run ended: "failure", "cancelled", ...
	Conclusion    string
	Attempt       int    // run_attempt: 1, then one more per re-run
	HeadRepo      string // head_repository.full_name
	Repo          string
	Path, HeadSHA string // immutable workflow-source evidence, event-dependent
	HeadBranch    string // head_branch: a branch or a tag name, GitHub does not say which
	HeadRepoID    int64  // head_repository.id: 0 when GitHub did not say
	// CallsOwnOnly: every reusable workflow the run called (referenced_workflows)
	// is a file of its own repository's .github/workflows/ at its own head
	// commit; true when it called none.
	CallsOwnOnly           bool
	HasReferencedWorkflows bool      // called workflows cannot be identified from a REST job display name
	RepoID                 int64     // repository.id: 0 when GitHub did not say
	Private                bool      // repository.private: false when GitHub did not say
	Visibility             string    // repository.visibility: "" when GitHub did not say
	Updated                time.Time // updated_at
}

// runJSON is a workflow run as GitHub answers it.
type runJSON struct {
	ID         int64             `json:"id"`
	Event      string            `json:"event"`
	Status     string            `json:"status"`
	Concl      string            `json:"conclusion"`
	Attempt    int               `json:"run_attempt"`
	Updated    time.Time         `json:"updated_at"`
	Path       string            `json:"path"`
	HeadSHA    string            `json:"head_sha"`
	Branch     string            `json:"head_branch"`
	Referenced []json.RawMessage `json:"referenced_workflows"`
	Head       *struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"head_repository"`
	Repository *struct {
		ID         int64  `json:"id"`
		FullName   string `json:"full_name"`
		Private    bool   `json:"private"`
		Visibility string `json:"visibility"`
	} `json:"repository"`
}

// run is r as a Run of repository fullName (GitHub's own name for it, when
// it gave one).
func (r runJSON) run(fullName string) Run {
	out := Run{ID: r.ID, Event: r.Event, Status: r.Status, Conclusion: r.Concl, Attempt: r.Attempt, Repo: fullName, Updated: r.Updated,
		Path: r.Path, HeadSHA: r.HeadSHA, HeadBranch: r.Branch, HasReferencedWorkflows: len(r.Referenced) > 0}

	if r.Repository != nil && r.Repository.FullName != "" {
		out.Repo, out.RepoID, out.Private, out.Visibility = r.Repository.FullName, r.Repository.ID, r.Repository.Private, r.Repository.Visibility
	}
	if r.Head != nil {
		out.HeadRepo, out.HeadRepoID = r.Head.FullName, r.Head.ID
	}
	out.CallsOwnOnly = callsOwnOnly(r.Referenced, out.Repo, r.HeadSHA)
	return out
}

// FromFork reports whether the run's code comes from outside Repo: a head
// repo other than Repo, or a pull_request* event with no head repo. It asks
// whether the head repo is Repo, not whether it is a fork: GitHub marks every
// run of a repository that is itself a fork (head_repository.fork) though its
// code is that repository's own, and a full name names one repository.
func (r Run) FromFork() bool {
	if r.HeadRepo == "" {
		return strings.HasPrefix(r.Event, "pull_request")
	}
	return !strings.EqualFold(r.HeadRepo, r.Repo)
}

// Run reads GET /repos/{o}/{r}/actions/runs/{id}.
func (c *Client) Run(ctx context.Context, installation int64, fullName string, runID int64) (Run, error) {
	if err := validRepo(fullName); err != nil {
		return Run{}, err
	}
	var r runJSON
	path := "/repos/" + fullName + "/actions/runs/" + strconv.FormatInt(runID, 10)
	if err := c.asInstall(ctx, installation, fullName, readPerms, http.MethodGet, path, nil, &r, 200); err != nil {
		return Run{}, err
	}
	return r.run(fullName), nil
}

// Job is a workflow job.
type Job struct {
	ID                             int64
	Status, Conclusion, RunnerName string
	RunID                          int64
	RunAttempt                     int    // the attempt of its run this job belongs to
	Name, StartedAt                string // display name (not YAML job key), and GitHub runtime start
}

// Started is when GitHub says the job started; false when it gives no
// start (a job not started yet) or one that does not parse.
func (j Job) Started() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, j.StartedAt)
	if err != nil || t.Unix() <= 0 {
		return time.Time{}, false
	}
	return t, true
}

// jobJSON is a workflow job as GitHub answers it.
type jobJSON struct {
	ID         int64    `json:"id"`
	Status     string   `json:"status"`
	Conclusion string   `json:"conclusion"`
	RunnerName string   `json:"runner_name"`
	RunID      int64    `json:"run_id"`
	RunAttempt int      `json:"run_attempt"`
	Labels     []string `json:"labels"`
	Name       string   `json:"name"`
	StartedAt  string   `json:"started_at"`
}

func (r jobJSON) job() Job {
	return Job{ID: r.ID, Status: r.Status, Conclusion: r.Conclusion, RunnerName: r.RunnerName, RunID: r.RunID, RunAttempt: r.RunAttempt, Name: r.Name, StartedAt: r.StartedAt}
}

// Job reads GET /repos/{o}/{r}/actions/jobs/{id}.
func (c *Client) Job(ctx context.Context, installation int64, fullName string, jobID int64) (Job, error) {
	if err := validRepo(fullName); err != nil {
		return Job{}, err
	}
	var r jobJSON
	path := "/repos/" + fullName + "/actions/jobs/" + strconv.FormatInt(jobID, 10)
	if err := c.asInstall(ctx, installation, fullName, readPerms, http.MethodGet, path, nil, &r, 200); err != nil {
		return Job{}, err
	}
	return r.job(), nil
}

// rerunPerms is what re-running a job needs: Actions write, on the job's
// repository alone.
var rerunPerms = Perms{"actions": "write", "metadata": "read"}

// ErrNoActionsWrite is a re-run GitHub refused because the App (or this
// installation of it, until its owner accepts the new permission) lacks
// Actions: write.
var ErrNoActionsWrite = errors.New("the GitHub App lacks the repository permission Actions: write")

// RerunJob asks GitHub to run a finished job again (and the jobs that need
// it): POST /repos/{o}/{r}/actions/jobs/{id}/rerun. GitHub takes it only once
// the job's run has completed; the job comes back queued under a new id in
// the run's next attempt.
func (c *Client) RerunJob(ctx context.Context, installation int64, fullName string, jobID int64) error {
	return c.rerun(ctx, installation, fullName, "/actions/jobs/"+strconv.FormatInt(jobID, 10)+"/rerun")
}

// RerunFailedJobs asks GitHub to run every failed or cancelled job of a
// completed run again: POST /repos/{o}/{r}/actions/runs/{id}/rerun-failed-jobs.
func (c *Client) RerunFailedJobs(ctx context.Context, installation int64, fullName string, runID int64) error {
	return c.rerun(ctx, installation, fullName, "/actions/runs/"+strconv.FormatInt(runID, 10)+"/rerun-failed-jobs")
}

// rerun posts a re-run. GitHub refuses a token Actions: write the App was not
// granted with 422 "The permissions requested are not granted to this
// installation", and the call itself with 403 "Resource not accessible by
// integration": both are ErrNoActionsWrite. Another 422 at the token (the
// repository is not in the installation) is its own error.
func (c *Client) rerun(ctx context.Context, installation int64, fullName, path string) error {
	if err := validRepo(fullName); err != nil {
		return err
	}
	err := c.asInstall(ctx, installation, fullName, rerunPerms, http.MethodPost, "/repos/"+fullName+path, map[string]any{}, nil, http.StatusCreated)
	var se *statusError
	if errors.As(err, &se) && (se.status == http.StatusUnprocessableEntity && strings.Contains(se.path, "/access_tokens") && strings.Contains(se.msg, "permissions") ||
		se.status == http.StatusForbidden && strings.Contains(se.msg, "not accessible")) {
		return fmt.Errorf("%w (%v)", ErrNoActionsWrite, err)
	}
	return err
}

// Scope is where runners register: a repository (owner/repo), the job's. The
// installation token is limited to it. Never an org: an org runner sits in a
// runner group every repo of the org may use.
type Scope struct {
	Repo string
}

func (s Scope) validate() error { return validRepo(s.Repo) }

// path returns "repos/{owner}/{repo}" ("" if invalid).
func (s Scope) path() string {
	if s.validate() != nil {
		return ""
	}
	return "repos/" + s.Repo
}

// perms is what registering, listing and deleting a repository's runners
// needs: Administration write (GitHub has no narrower one), on that
// repository alone.
func (s Scope) perms() Perms { return Perms{"administration": "write", "metadata": "read"} }

// JITConfig registers a just-in-time runner and returns its id and the encoded
// config for the runner binary. group 0 means the default group (1).
func (c *Client) JITConfig(ctx context.Context, installation int64, s Scope, name string, labels []string, group int64) (int64, string, error) {
	if err := s.validate(); err != nil {
		return 0, "", err
	}
	if name == "" {
		return 0, "", errors.New("ghapp: runner name is empty")
	}
	if group == 0 {
		group = defaultGroup
	}
	if labels == nil {
		labels = []string{}
	}
	in := map[string]any{"name": name, "runner_group_id": group, "labels": labels, "work_folder": "_work"}
	var r struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		Encoded string `json:"encoded_jit_config"`
	}
	path := "/" + s.path() + "/actions/runners/generate-jitconfig"
	if err := c.asInstall(ctx, installation, s.Repo, s.perms(), http.MethodPost, path, in, &r, http.StatusCreated); err != nil {
		return 0, "", err
	}
	return r.Runner.ID, r.Encoded, nil
}

// Runner is a registered self-hosted runner.
type Runner struct {
	ID     int64
	Name   string
	Status string
	Busy   bool
}

// Runners lists every runner in the scope whose name starts with namePrefix.
func (c *Client) Runners(ctx context.Context, installation int64, s Scope, namePrefix string) ([]Runner, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	var out []Runner
	for page, seen := 1, 0; ; page++ {
		var r struct {
			Total   int `json:"total_count"`
			Runners []struct {
				ID     int64  `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				Busy   bool   `json:"busy"`
			} `json:"runners"`
		}
		path := "/" + s.path() + "/actions/runners?per_page=100&page=" + strconv.Itoa(page)
		if err := c.asInstall(ctx, installation, s.Repo, s.perms(), http.MethodGet, path, nil, &r, 200); err != nil {
			return nil, err
		}
		for _, x := range r.Runners {
			if strings.HasPrefix(x.Name, namePrefix) {
				out = append(out, Runner{x.ID, x.Name, x.Status, x.Busy})
			}
		}
		seen += len(r.Runners)
		if len(r.Runners) == 0 || seen >= r.Total || page >= 1000 {
			return out, nil
		}
	}
}

// DeleteRunner removes a runner; already gone (404) counts as success.
func (c *Client) DeleteRunner(ctx context.Context, installation int64, s Scope, id int64) error {
	if err := s.validate(); err != nil {
		return err
	}
	path := "/" + s.path() + "/actions/runners/" + strconv.FormatInt(id, 10)
	return c.asInstall(ctx, installation, s.Repo, s.perms(), http.MethodDelete, path, nil, nil, http.StatusNoContent, http.StatusNotFound)
}

// UserInstallations returns the installation ids a user token can see.
func UserInstallations(ctx context.Context, hc *http.Client, apiURL, userToken string) ([]int64, error) {
	var ids []int64
	for page, seen := 1, 0; ; page++ {
		var r struct {
			Total         int `json:"total_count"`
			Installations []struct {
				ID int64 `json:"id"`
			} `json:"installations"`
		}
		path := "/user/installations?per_page=100&page=" + strconv.Itoa(page)
		if err := call(ctx, hc, http.MethodGet, apiURL, path, userToken, nil, &r, 200); err != nil {
			return nil, err
		}
		for _, i := range r.Installations {
			ids = append(ids, i.ID)
		}
		seen += len(r.Installations)
		if len(r.Installations) == 0 || seen >= r.Total || page >= 1000 {
			return ids, nil
		}
	}
}

// Manifest describes the App to register through the manifest flow.
type Manifest struct {
	Name, URL, HookURL, RedirectURL, Description string
	// SetupURL is where GitHub sends the person after installing the App,
	// with installation_id (a claim anyone can type: never proof alone).
	SetupURL     string
	CallbackURLs []string
	Public       bool
}

// JSON renders the manifest document GitHub expects.
func (m Manifest) JSON() ([]byte, error) {
	if m.Name == "" || m.URL == "" || m.HookURL == "" {
		return nil, errors.New("ghapp: manifest needs Name, URL and HookURL")
	}
	return json.Marshal(map[string]any{
		"name":                     m.Name,
		"url":                      m.URL,
		"description":              m.Description,
		"redirect_url":             m.RedirectURL,
		"setup_url":                m.SetupURL,
		"callback_urls":            append([]string{}, m.CallbackURLs...),
		"public":                   m.Public,
		"hook_attributes":          map[string]any{"url": m.HookURL, "active": true},
		"request_oauth_on_install": false,
		"setup_on_update":          false,
		"default_events":           []string{"workflow_job"},
		"default_permissions":      m.permissions(),
	})
}

// permissions: runners register per repository (an org runner's group lets
// every repo of the org use it), which takes Administration write: GitHub has
// nothing narrower, and each token limits it to the job's repository. Members
// read is for the link proof: UserOrgAdmin's GET /user/memberships/orgs/{org}
// needs it on the user token. It is an organization permission, so an
// installation on a user account gets nothing from it. Actions write is for
// re-running a job whose box was taken back while it ran (RerunJob); reading
// runs and jobs asks a token for read alone. Contents read is for the workflow
// at the run's immutable revision; its token has no runner administration.
func (m Manifest) permissions() map[string]string {
	return map[string]string{"actions": "write", "metadata": "read", "administration": "write", "members": "read", "contents": "read"}
}

// Created is the result of converting a manifest code.
type Created struct {
	ID                                                 int64
	Slug, Owner, ClientID, ClientSecret, WebhookSecret string
	PEM                                                []byte
}

var codeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

// Convert exchanges the one-time manifest code for the new App's credentials.
func Convert(ctx context.Context, hc *http.Client, apiURL, code string) (Created, error) {
	if !codeRE.MatchString(code) {
		return Created{}, errors.New("ghapp: invalid manifest code")
	}
	var r struct {
		ID    int64  `json:"id"`
		Slug  string `json:"slug"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		WebhookSecret string `json:"webhook_secret"`
		PEM           string `json:"pem"`
	}
	if err := call(ctx, hc, http.MethodPost, apiURL, "/app-manifests/"+code+"/conversions", "", nil, &r, http.StatusCreated); err != nil {
		return Created{}, err
	}
	return Created{r.ID, r.Slug, r.Owner.Login, r.ClientID, r.ClientSecret, r.WebhookSecret, []byte(r.PEM)}, nil
}

// VerifyHook checks X-Hub-Signature-256 ("sha256=<hex>") over the raw body.
func VerifyHook(secret, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// UserLogin is the signed-in person's login (GET /user).
func UserLogin(ctx context.Context, hc *http.Client, apiURL, userToken string) (string, error) {
	var r struct {
		Login string `json:"login"`
	}
	if err := call(ctx, hc, http.MethodGet, apiURL, "/user", userToken, nil, &r, 200); err != nil {
		return "", err
	}
	return r.Login, nil
}

// ErrNoMembersRead is GitHub refusing the membership read because the App
// lacks Members read, or the org's installation has not accepted it.
var ErrNoMembersRead = errors.New("the runner App lacks the Members: read permission, so GitHub will not say who administers the org. " +
	"Add it in the App's settings (Permissions & events > Organization permissions > Members: Read-only), " +
	"accept the new permission on the org's installation (org settings > GitHub Apps), then link again")

// UserOrgAdmin says whether the signed-in person is an active admin of org
// (GET /user/memberships/orgs/{org}). Not a member at all (404) is false, not
// an error. A 403 is an error: "Resource not accessible by integration" is the
// App missing Members read (ErrNoMembersRead); otherwise the org blocks the App.
func UserOrgAdmin(ctx context.Context, hc *http.Client, apiURL, userToken, org string) (bool, error) {
	if !validName(org) {
		return false, fmt.Errorf("org %q is not a GitHub name", org)
	}
	var r struct {
		State string `json:"state"`
		Role  string `json:"role"`
	}
	err := call(ctx, hc, http.MethodGet, apiURL, "/user/memberships/orgs/"+org, userToken, nil, &r, 200)
	var se *statusError
	switch {
	case errors.As(err, &se) && se.status == http.StatusNotFound:
		return false, nil
	case errors.As(err, &se) && se.status == http.StatusForbidden && strings.Contains(se.msg, "not accessible by integration"):
		return false, fmt.Errorf("%w (GitHub: %d %s)", ErrNoMembersRead, se.status, se.msg)
	case err != nil:
		return false, err
	}
	return r.State == "active" && r.Role == "admin", nil
}
