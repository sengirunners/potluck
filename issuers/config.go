package issuers

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/secrets"
)

// Spec is one validated issuer of config.json's secrets.issuers.
type Spec struct {
	Name       string
	Kind       string // KindGitHubApp or KindPotluck
	Use        string // "" or UseRunners
	As         string // KindPotluck: the identity a GitHub job's token acts as ("": jobs get none)
	AppID      int64
	PrivateKey string // a secret reference, never a value
	APIURL     string
	Ceiling    []Rule
}

// Rule is one ceiling rule: a repository (owner/name, or owner/*), the most
// each permission may be, optionally the workflow files of the default branch
// it serves (then only GitHub jobs, never an identity), the job events it
// serves, the elevated permissions it may grant at write, and why.
//
// A potluck rule is instead a project (slug or ULID), an area of it ("": any)
// and the most a token's role may be; Workflows and Events limit it as above.
type Rule struct {
	Project     string
	Area        string
	Role        string
	Repo        string
	Permissions map[string]string
	Workflows   []string
	Events      []string // jobrule.RuleEvents: default push and schedule
	Elevated    []string
	Reason      string
}

// Issuer config kinds and uses.
const (
	KindGitHubApp = "github-app" // mints KindGitHubToken
	KindPotluck   = "potluck"    // mints KindPotluckToken
	UseRunners    = "runners.github"
)

var (
	issuerNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	ghNameRE     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	permNameRE   = regexp.MustCompile(`^[a-z][a-z_]{0,63}$`)
)

// Permissions are an allowlist in three tiers. Names and levels are GitHub's
// "Create an installation access token for an app"
// (https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app)
// and "Permissions required for GitHub Apps"
// (https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps).
//
// Plain: read or write under any rule naming them.
var plainPerms = []string{"contents", "pull_requests", "issues", "discussions", "metadata"}

// Elevated: read like plain; write only when the rule lists the permission
// under elevated and gives a reason, and the request gives one too.
// workflows is write only at GitHub (there is no workflows read), so it is
// always elevated; write to workflow files amounts to reading every secret a
// workflow can name.
var elevatedPerms = []string{"workflows", "actions", "checks", "statuses", "deployments", "packages", "attestations", "pages",
	"security_events", "secret_scanning_alerts", "vulnerability_alerts", "merge_queues"}

// Everything else is denied, among them: administration, secrets,
// environments, repository_hooks, dependabot_secrets, codespaces,
// repository_custom_properties, members, every organization_* and user
// permission (email_addresses, followers, git_ssh_keys, gpg_keys, ...), and
// any name GitHub adds later. actions_variables, codespaces_secrets and
// team_discussions are not names GitHub's token call takes at all (checked
// against the page above, 2026-11-10); they are refused like any unknown name.

// permTier answers "plain", "elevated" or "" (denied).
func permTier(p string) string {
	switch {
	case slices.Contains(plainPerms, p):
		return "plain"
	case slices.Contains(elevatedPerms, p):
		return "elevated"
	}
	return ""
}

// checkLevel refuses a level GitHub does not take for p.
func checkLevel(p, lvl string) error {
	if lvl != "read" && lvl != "write" {
		return fmt.Errorf("permission %s=%q: read or write", p, lvl)
	}
	if p == "workflows" && lvl != "write" {
		return fmt.Errorf("permission workflows is write only at GitHub")
	}
	return nil
}

// elevatedWrite is the permissions of p that are elevated at write, sorted.
func elevatedWrite(p map[string]string) []string {
	var out []string
	for k, v := range p {
		if v == "write" && permTier(k) == "elevated" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Specs validates config.json's secrets.issuers. Any error refuses the
// whole section: an issuer the daemon cannot read as safe mints nothing.
func Specs(f config.File) (map[string]Spec, error) {
	if err := f.Secrets.Err(); err != nil {
		return nil, err
	}
	out := map[string]Spec{}
	for name, is := range f.Secrets.Issuers {
		s, err := spec(name, is, f.Runners.GitHub)
		if err != nil {
			return nil, err
		}
		out[name] = s
	}
	return out, nil
}

func spec(name string, is config.SecretsIssuer, broker config.RunnersGitHub) (Spec, error) {
	bad := func(format string, a ...any) (Spec, error) {
		return Spec{}, fmt.Errorf("secrets.issuers.%s: %s", name, fmt.Sprintf(format, a...))
	}
	if !issuerNameRE.MatchString(name) {
		return Spec{}, fmt.Errorf("secrets.issuers: %q is not an issuer name (lower case letters, digits and -)", name)
	}
	switch is.Kind {
	case KindGitHubApp:
	case KindPotluck:
		return potluckSpec(name, is)
	default:
		return bad("kind %q: %q or %q", is.Kind, KindGitHubApp, KindPotluck)
	}
	if is.As != "" {
		return bad("as is a %q issuer's", KindPotluck)
	}
	s := Spec{Name: name, Kind: is.Kind, Use: is.Use, AppID: is.AppID, PrivateKey: is.PrivateKey, APIURL: is.APIURL}
	switch is.Use {
	case "":
	case UseRunners:
		if is.AppID != 0 || is.PrivateKey != "" || is.APIURL != "" {
			return bad("use %q takes the broker's App: leave app_id, private_key and api_url unset", UseRunners)
		}
		s.AppID, s.PrivateKey, s.APIURL = broker.AppID, broker.PrivateKey, broker.APIURL
		if s.AppID == 0 {
			return bad("use %q but runners.github has no app_id", UseRunners)
		}
	default:
		return bad("use %q: the only use is %q", is.Use, UseRunners)
	}
	if s.AppID <= 0 {
		return bad("app_id must be a positive GitHub App id")
	}
	if err := keyRef(s.PrivateKey); err != nil {
		return bad("private_key: %v", err)
	}
	if s.APIURL == "" {
		s.APIURL = "https://api.github.com"
	}
	s.APIURL = strings.TrimRight(s.APIURL, "/")
	if err := apiURL(s.APIURL); err != nil {
		return bad("api_url: %v", err)
	}
	for i, r := range is.Ceiling {
		rule, err := checkRule(r)
		if err != nil {
			return bad("ceiling[%d]: %v", i, err)
		}
		s.Ceiling = append(s.Ceiling, rule)
	}
	return s, nil
}

// keyRef refuses anything but a secret reference: never an inline key.
func keyRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("a secret reference (file:, keychain:, env:) is required")
	}
	if strings.Contains(ref, "-----BEGIN") || strings.ContainsAny(ref, "\n\r") {
		return fmt.Errorf("an inline key is refused: name it by a secret reference (file:, keychain:, env:)")
	}
	if _, err := secrets.Parse(ref); err != nil {
		return fmt.Errorf("not a secret reference (file:, keychain:, env:)")
	}
	return nil
}

// apiURL is https, or http to a loopback address (a GitHub stand-in on this
// machine).
func apiURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q is not an API base URL", v)
	}
	if u.Scheme == "https" {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" && ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%q: https only (http only to a loopback address)", v)
}

func checkRule(r config.SecretsCeiling) (Rule, error) {
	if r.Project != "" || r.Area != "" || r.Role != "" {
		return Rule{}, fmt.Errorf("repo %q: project, area and role are a %q issuer's", r.Repo, KindPotluck)
	}
	o, n, ok := strings.Cut(r.Repo, "/")
	if !ok || !ghNameRE.MatchString(o) || strings.Contains(o, "..") || (n != "*" && (!ghNameRE.MatchString(n) || strings.Contains(n, ".."))) {
		return Rule{}, fmt.Errorf("repo %q: owner/name or owner/*", r.Repo)
	}
	if len(r.Permissions) == 0 {
		return Rule{}, fmt.Errorf("repo %s: no permissions (a rule that grants nothing is no rule)", r.Repo)
	}
	perms := map[string]string{}
	for p, lvl := range r.Permissions {
		if permTier(p) == "" {
			return Rule{}, fmt.Errorf("permission %q is never minted for a box (not on the allowlist)", p)
		}
		if err := checkLevel(p, lvl); err != nil {
			return Rule{}, err
		}
		perms[p] = lvl
	}
	for _, p := range r.Elevated {
		if permTier(p) != "elevated" || perms[p] != "write" {
			return Rule{}, fmt.Errorf("elevated %q: an elevated permission the rule grants at write", p)
		}
	}
	for _, p := range elevatedWrite(perms) {
		if !slices.Contains(r.Elevated, p) {
			return Rule{}, fmt.Errorf("permission %s=write is elevated: list it under \"elevated\" and give a \"reason\"", p)
		}
	}
	if len(r.Elevated) > 0 && strings.TrimSpace(r.Reason) == "" {
		return Rule{}, fmt.Errorf("repo %s: a rule with elevated permissions needs a reason", r.Repo)
	}
	var wfs []string
	for _, w := range r.Workflows {
		if !jobrule.ValidWorkflow(w) {
			return Rule{}, fmt.Errorf("workflow %q: a file under .github/workflows/ (.yml or .yaml)", w)
		}
		wfs = append(wfs, w)
	}
	events, err := jobrule.RuleEvents(r.Events)
	if err != nil {
		return Rule{}, err
	}
	return Rule{Repo: r.Repo, Permissions: perms, Workflows: wfs, Events: events, Elevated: slices.Clone(r.Elevated), Reason: r.Reason}, nil
}

// matches says whether the rule covers repo for sub.
func (r Rule) matches(repo string, sub Subject) bool {
	o, n, _ := strings.Cut(r.Repo, "/")
	ro, rn, _ := strings.Cut(repo, "/")
	if !strings.EqualFold(o, ro) || (n != "*" && !strings.EqualFold(n, rn)) {
		return false
	}
	if sub.Job != nil {
		return jobrule.Serves(r.Events, r.Workflows, *sub.Job)
	}
	return len(r.Workflows) == 0 // a workflow rule serves only jobs
}

// covers says whether every permission asked is within the rule. Metadata
// read is GitHub's own on every repository token, so always within.
func (r Rule) covers(asked map[string]string) bool {
	for p, lvl := range asked {
		if p == "metadata" && lvl == "read" {
			continue
		}
		top, ok := r.Permissions[p]
		if !ok || checkLevel(p, lvl) != nil || (lvl == "write" && top != "write") {
			return false
		}
		if lvl == "write" && permTier(p) == "elevated" && !slices.Contains(r.Elevated, p) {
			return false
		}
	}
	return true
}

// match finds a single rule covering the request: permissions are never
// added up across rules.
func (s Spec) match(repo string, sub Subject, asked map[string]string) (Rule, bool) {
	for _, r := range s.Ceiling {
		if r.matches(repo, sub) && r.covers(asked) {
			return r, true
		}
	}
	return Rule{}, false
}

// permString is a permission map in a fixed order: "contents=write,metadata=read".
func permString(p map[string]string) string {
	ks := make([]string, 0, len(p))
	for k, v := range p {
		ks = append(ks, k+"="+v)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// permMap reads permString's form back (nil for "").
func permMap(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}
