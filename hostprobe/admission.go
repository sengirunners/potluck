package hostprobe

// Admission on a shared host (zm-eakh.12.7.7): zm host share and zm pool
// host bind zm-run-tart's admission keys (admission_url, admission_token_file,
// admission_ttl_s; sengirunners/potluck cmd/potluck-driver-tart/README.md "Sharing a host with other
// work") from --admission-url, --admission-token-file and --admission-ttl-s,
// and a re-run without them keeps an earlier binding's, so an owner never
// hand-edits a binding and a later share never drops them. Conformance runs
// without them: the owner is running share and joining comes first, so its
// check boxes take no lease, while every box lent does. A controller that
// counts the boxes it sees still counts them.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// NoAdmission is --admission-url none: the binding carries no admission keys.
const NoAdmission = "none"

// bootTimeoutS is zm-run-tart's default boot_timeout_s, which share never
// binds: a lease must outlast the boot by 30 s (vmhost's config.go).
const bootTimeoutS = 90

var admissionKeys = []string{"admission_url", "admission_token_file", "admission_ttl_s"}

// admissionConfig is the admission keys share binds (nil: none; share's own
// conformance runs without them) and the earlier binding they were kept
// from ("": the flags).
// A URL flag takes the flags alone; without one, an earlier binding's keys
// stay, each flag given replacing its own. Checked as zm-run-tart checks
// them, which hostprobe may not import.
func admissionConfig(o ShareOptions) (map[string]string, string, error) {
	cfg, from := map[string]string{}, ""
	if o.AdmissionURL != NoAdmission { // fail closed: keys to keep, or a keyless binding to unbind, unseen
		if err := earlierSeen(o); err != nil {
			return nil, "", err
		}
	}
	switch o.AdmissionURL {
	case NoAdmission:
		if o.AdmissionTokenFile != "" || o.AdmissionTTLS != 0 {
			return nil, "", errors.New("--admission-url none removes the admission keys: drop --admission-token-file and --admission-ttl-s")
		}
		return nil, "", nil
	case "":
		e, name, err := earlierAdmission(o)
		if err != nil {
			return nil, "", err
		}
		if e == nil {
			if o.AdmissionTokenFile != "" || o.AdmissionTTLS != 0 {
				return nil, "", errors.New("--admission-token-file and --admission-ttl-s need --admission-url URL")
			}
			return nil, "", nil
		}
		for _, k := range admissionKeys {
			if e[k] != "" {
				cfg[k] = e[k]
			}
		}
		from = name
	default:
		cfg["admission_url"] = o.AdmissionURL
	}
	if o.AdmissionTokenFile != "" {
		p, err := filepath.Abs(o.AdmissionTokenFile) // the driver runs elsewhere
		if err != nil {
			return nil, "", fmt.Errorf("--admission-token-file: %v", err)
		}
		cfg["admission_token_file"] = p
	}
	if o.AdmissionTTLS != 0 {
		cfg["admission_ttl_s"] = strconv.Itoa(o.AdmissionTTLS)
	}
	if err := checkAdmission(cfg); err != nil {
		if from != "" {
			err = fmt.Errorf("%v (kept from %s: pass --admission-url URL to change it, or none to remove it)", err, from)
		}
		return nil, "", err
	}
	return cfg, from, nil
}

// earlierAdmission is the admission keys of this share's earlier box
// bindings on this machine (<prefix>-<size>), and the first one's name.
func earlierAdmission(o ShareOptions) (map[string]string, string, error) {
	var got map[string]string
	name := ""
	for _, t := range o.Lent() {
		if t.Config["admission_url"] == "" {
			continue
		}
		if got == nil {
			got, name = t.Config, t.Name
			continue
		}
		for _, k := range admissionKeys {
			if got[k] != t.Config[k] {
				return nil, "", fmt.Errorf("bindings %s and %s differ in %s: pass --admission-url URL (or none) to say which", name, t.Name, k)
			}
		}
	}
	return got, name, nil
}

// earlierSeen refuses a run that cannot see the bindings it replaces
// (o.Earlier, in the scope it binds in): without --admission-url it would
// drop their keys; with a URL, a keyless one could stay live beside it.
func earlierSeen(o ShareOptions) error {
	fix := "pass --admission-url URL or none, so a re-run never drops admission keys an earlier share bound"
	if o.AdmissionURL != "" {
		fix = "try again, or pass --admission-url none: share could not tell whether a binding without admission keys stays live beside the new ones"
	}
	if o.EarlierUnread != "" {
		return fmt.Errorf("could not read this machine's bindings (%s): %s", o.EarlierUnread, fix)
	}
	for _, t := range o.Lent() {
		if t.Config == nil { // every binding share makes has config: this view hides it
			return fmt.Errorf("binding %s shows no config (only the machine's owner sees it): %s", t.Name, fix)
		}
	}
	return nil
}

// checkAdmission mirrors zm-run-tart's checks of its admission keys.
func checkAdmission(cfg map[string]string) error {
	u, err := url.Parse(cfg["admission_url"])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return fmt.Errorf("admission URL %q: want an http(s) URL with no credentials or query (a token goes in --admission-token-file)", cfg["admission_url"])
	}
	if v := cfg["admission_ttl_s"]; v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < bootTimeoutS+30 || n > 3600 {
			return fmt.Errorf("admission TTL %q: want %d..3600 seconds (outlasting the boot by 30 s)", v, bootTimeoutS+30)
		}
	}
	if p := cfg["admission_token_file"]; p != "" {
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("admission token file: %v", err)
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return fmt.Errorf("admission token file: %v", err)
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("admission token file %s must be a 0600 file (chmod 600 %s)", p, p)
		}
	}
	return nil
}

// unbinder is optional on a Binder (computeclient.Owner has it).
type unbinder interface {
	UnbindRunTarget(ctx context.Context, target string) error
	UnbindHostRunTarget(ctx context.Context, target string) error
}

// dropKeyless is for a size that stayed unbound in a run with admission
// keys while its earlier binding has none: that binding's boxes take room
// the controller does not count, so it is unbound where Bind binds, and the
// note says so; else the note says it is live WITHOUT admission. ok false:
// no such binding (or none where Bind binds), and nothing to say.
func dropKeyless(ctx context.Context, b Binder, o ShareOptions, res ShareResult, name string) (note string, ok bool) {
	if res.Admission == nil || !slices.ContainsFunc(o.Earlier, func(t computeapi.RunTarget) bool {
		return t.Name == name && t.Kind == "box" && t.Host == "" && t.Config != nil && t.Config["admission_url"] == ""
	}) {
		return "", false
	}
	flag, why := "", ""
	if o.Pool != "" {
		flag = "--host "
	}
	if u, isU := b.(unbinder); isU {
		var err error
		if o.Pool != "" {
			err = u.UnbindHostRunTarget(ctx, name)
		} else {
			err = u.UnbindRunTarget(ctx, name)
		}
		var ae *api.Error
		switch {
		case err == nil:
			return "; " + name + " from before had no admission keys, so it was unbound (its boxes took room the controller does not count)", true
		case errors.As(err, &ae) && ae.Code == api.CodeNotFound:
			return "", false
		}
		why = fmt.Sprintf(" (unbind: %v)", err)
	}
	return fmt.Sprintf("; %s is still bound from before WITHOUT admission: its boxes are uncounted by the controller%s; run again, or %s", name, why, config.Command("target unbind", flag+name)), true
}
