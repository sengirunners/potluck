package hostprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
)

// tokenFile is a 0600 admission token file.
func tokenFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "admission.token")
	if err := os.WriteFile(p, []byte("s3cret\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func admissionOptions() ShareOptions {
	return ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", Sizes: []string{"c2m4", "c4m16"}, Driver: "ref"}
}

// zm-eakh.12.7.7: --admission-url, --admission-token-file and
// --admission-ttl-s go into every size's binding, and into its conformance
// run, whose boxes use the host's room as much as a renter's.
func TestShareAdmissionFlagsBindEverySize(t *testing.T) {
	tok := tokenFile(t, 0o600)
	o := admissionOptions()
	o.AdmissionURL, o.AdmissionTokenFile, o.AdmissionTTLS = "https://ci.example:8443/zm", tok, 300
	run := &fakeRunner{on: func(line string) (Result, bool) { return Result{Stdout: "PASS"}, true }}
	var bound []computeapi.RunTarget
	b := binderFunc(func(_ context.Context, r computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
		bound = append(bound, r.Target)
		return computeapi.RunTargetView{}, nil
	})
	res, err := shareOn(context.Background(), run, b, o)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"admission_url": "https://ci.example:8443/zm", "admission_token_file": tok, "admission_ttl_s": "300"}
	if len(bound) != 2 {
		t.Fatalf("bound %d sizes, want 2", len(bound))
	}
	for _, tg := range bound {
		for k, v := range want {
			if tg.Config[k] != v {
				t.Errorf("%s: %s=%q, want %q", tg.Name, k, tg.Config[k], v)
			}
		}
	}
	for _, s := range res.Sizes {
		args := strings.Join(s.Conformance, " ")
		for k, v := range want {
			if !strings.Contains(args, "--config "+k+"="+v) {
				t.Errorf("%s conformance has no %s: %s", s.Binding, k, args)
			}
		}
	}
	if res.Admission["admission_url"] != want["admission_url"] || res.AdmissionFrom != "" {
		t.Errorf("result: %v from %q", res.Admission, res.AdmissionFrom)
	}

	// A relative token file is bound as an absolute path: the driver runs
	// in another directory.
	dir := filepath.Dir(tok)
	t.Chdir(dir)
	o.AdmissionTokenFile, o.DryRun = filepath.Base(tok), true
	if res, err = shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil || res.Sizes[0].Config["admission_token_file"] != tok {
		t.Errorf("relative token file: %v %v", err, res.Sizes)
	}

	// No flag and nothing earlier: no admission keys at all.
	o = admissionOptions()
	o.DryRun = true
	if res, err = shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil || res.Admission != nil {
		t.Fatalf("%v %v", err, res.Admission)
	}
	for _, s := range res.Sizes {
		for k := range s.Config {
			if strings.HasPrefix(k, "admission") {
				t.Errorf("%s binds %s without the flag", s.Binding, k)
			}
		}
	}
}

// A re-run without the flags keeps what an earlier share bound; a flag
// given replaces that key; --admission-url none removes them all.
func TestShareAdmissionKeptOnRerun(t *testing.T) {
	tok := tokenFile(t, 0o600)
	earlier := []computeapi.RunTarget{
		{Name: "h-c2m4", Kind: "box", Config: map[string]string{"admission_url": "http://10.0.0.5:9000", "admission_token_file": tok, "admission_ttl_s": "200"}},
		{Name: "other-c2m4", Kind: "box", Config: map[string]string{"admission_url": "http://elsewhere.example"}}, // another prefix
		{Name: "h-c2m4@far", Kind: "box", Host: "far", Config: map[string]string{"admission_url": "http://far.example"}},
		{Name: "h-repo", Kind: "repo", Config: map[string]string{"admission_url": "http://repo.example"}},
	}
	o := admissionOptions()
	o.DryRun, o.Earlier = true, earlier
	res, err := shareOn(context.Background(), &fakeRunner{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Sizes {
		if s.Config["admission_url"] != "http://10.0.0.5:9000" || s.Config["admission_token_file"] != tok || s.Config["admission_ttl_s"] != "200" {
			t.Errorf("%s did not keep the admission keys: %v", s.Binding, s.Config)
		}
	}
	if res.AdmissionFrom != "h-c2m4" {
		t.Errorf("kept from %q", res.AdmissionFrom)
	}

	o.AdmissionTTLS = 600
	if res, err = shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil ||
		res.Sizes[1].Config["admission_ttl_s"] != "600" || res.Sizes[1].Config["admission_url"] != "http://10.0.0.5:9000" {
		t.Errorf("--admission-ttl-s on a re-run: %v %v", err, res.Sizes)
	}

	o.AdmissionTTLS, o.AdmissionURL = 0, NoAdmission
	if res, err = shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil || res.Admission != nil {
		t.Fatalf("none: %v %v", err, res.Admission)
	}
	for _, s := range res.Sizes {
		if s.Config["admission_url"] != "" || s.Config["admission_token_file"] != "" || s.Config["admission_ttl_s"] != "" {
			t.Errorf("none kept %v", s.Config)
		}
	}

	// A new URL takes the flags only: an earlier token was for another controller.
	o.AdmissionURL = "https://new.example"
	if res, err = shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil ||
		res.Sizes[0].Config["admission_url"] != "https://new.example" || res.Sizes[0].Config["admission_token_file"] != "" || res.Sizes[0].Config["admission_ttl_s"] != "" {
		t.Errorf("a new URL: %v %v", err, res.Sizes)
	}

	// Earlier bindings that disagree: the owner says which.
	o.AdmissionURL = ""
	o.Earlier = append(earlier, computeapi.RunTarget{Name: "h-c4m16", Kind: "box", Config: map[string]string{"admission_url": "http://10.0.0.6:9000"}})
	if _, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err == nil || !strings.Contains(err.Error(), "--admission-url") {
		t.Errorf("disagreeing bindings: %v", err)
	}
}

func TestShareAdmissionRefusals(t *testing.T) {
	tok, open := tokenFile(t, 0o600), tokenFile(t, 0o644)
	for name, c := range map[string]struct {
		url, tok string
		ttl      int
		earlier  map[string]string
		want     string
	}{
		"ftp":                {url: "ftp://ci.example", want: "http(s)"},
		"no host":            {url: "http://", want: "http(s)"},
		"credentials":        {url: "https://u:p@ci.example", want: "credentials"},
		"query":              {url: "https://ci.example/?token=x", want: "query"},
		"token not 0600":     {url: "https://ci.example", tok: open, want: "0600"},
		"token missing":      {url: "https://ci.example", tok: filepath.Join(t.TempDir(), "nope"), want: "admission"},
		"ttl too short":      {url: "https://ci.example", ttl: 60, want: "120..3600"},
		"ttl too long":       {url: "https://ci.example", ttl: 4000, want: "120..3600"},
		"ttl negative":       {url: "https://ci.example", ttl: -1, want: "120..3600"},
		"token without url":  {tok: tok, want: "--admission-url"},
		"ttl without url":    {ttl: 300, want: "--admission-url"},
		"none with token":    {url: NoAdmission, tok: tok, want: "none"},
		"kept token not 600": {earlier: map[string]string{"admission_url": "https://ci.example", "admission_token_file": open}, want: "kept from h-c2m4"},
		"kept bad ttl":       {earlier: map[string]string{"admission_url": "https://ci.example", "admission_ttl_s": "soon"}, want: "kept from h-c2m4"},
	} {
		o := admissionOptions()
		o.AdmissionURL, o.AdmissionTokenFile, o.AdmissionTTLS = c.url, c.tok, c.ttl
		if c.earlier != nil {
			o.Earlier = []computeapi.RunTarget{{Name: "h-c2m4", Kind: "box", Config: c.earlier}}
		}
		run := &fakeRunner{}
		if _, err := shareOn(context.Background(), run, binderFunc(nil), o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
		if len(run.calls) != 0 {
			t.Errorf("%s ran %v", name, run.calls)
		}
		// Precheck refuses it too, so share never asks for root first.
		if err := precheckOn(context.Background(), nil, o); err == nil {
			t.Errorf("%s: Precheck accepted", name)
		}
	}
}

// Fail closed (review of #552): without --admission-url, a re-run whose read
// of this machine's bindings failed, or showed a binding without its config
// (a non-owner's view), refuses rather than replace the bindings without
// their keys.
func TestShareAdmissionUnreadRefuses(t *testing.T) {
	o := admissionOptions()
	o.EarlierUnread = "permission denied"
	for _, url := range []string{""} {
		o.AdmissionURL = url
		if _, err := shareOn(context.Background(), &fakeRunner{}, binderFunc(nil), o); err == nil ||
			!strings.Contains(err.Error(), "could not read this machine's bindings (permission denied): pass --admission-url URL or none") {
			t.Errorf("unread: %v", err)
		}
		if err := precheckOn(context.Background(), nil, o); err == nil {
			t.Error("unread: Precheck accepted")
		}
	}
	o.AdmissionTTLS = 300
	if _, err := shareOn(context.Background(), &fakeRunner{}, binderFunc(nil), o); err == nil || !strings.Contains(err.Error(), "could not read") {
		t.Errorf("unread with --admission-ttl-s: %v", err)
	}
	o.AdmissionTTLS = 0
	o.AdmissionURL, o.DryRun = NoAdmission, true
	if _, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil {
		t.Errorf("unread with --admission-url none: %v", err)
	}
	// With a URL: a keyless earlier binding it cannot see could stay live.
	o.AdmissionURL = "https://ci.example"
	if _, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err == nil || !strings.Contains(err.Error(), "--admission-url none") {
		t.Errorf("unread with a URL: %v", err)
	}
	o = admissionOptions()
	o.DryRun, o.Earlier = true, []computeapi.RunTarget{{Name: "h-c4m16", Kind: "box"}}
	if _, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err == nil || !strings.Contains(err.Error(), "h-c4m16") || !strings.Contains(err.Error(), "--admission-url URL or none") {
		t.Errorf("config not shown: %v", err)
	}
}

// A controller with no room during conformance is said in plain words, and
// the other sizes are not blamed on the security checks.
func TestShareAdmissionNoRoomSaysSo(t *testing.T) {
	o := admissionOptions()
	o.AdmissionURL = "https://ci.example"
	run := &fakeRunner{on: func(line string) (Result, bool) {
		if strings.HasPrefix(line, "zm-run-conformance ") {
			return Result{Exit: 1, Stdout: "FAIL  handles: acquire: unavailable: no room on this host now: ci jobs (retry in 30 s)"}, true
		}
		return Result{}, true
	}}
	res, err := shareOn(context.Background(), run, binderFunc(nil), o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Sizes[0].Error, "the admission controller had no room now; try again later") {
		t.Errorf("first size: %q", res.Sizes[0].Error)
	}
	if e := res.Sizes[1].Error; strings.Contains(e, "security checks failed") || !strings.Contains(e, "no room") {
		t.Errorf("second size: %q", e)
	}
}

type unbindRecorder struct {
	binderFunc
	project, host []string
	err           error
}

func (u *unbindRecorder) UnbindRunTarget(_ context.Context, name string) error {
	u.project = append(u.project, name)
	return u.err
}

func (u *unbindRecorder) UnbindHostRunTarget(_ context.Context, name string) error {
	u.host = append(u.host, name)
	return u.err
}

// Adding admission to a lent host whose new run fails: the old binding
// without the keys would keep taking room the controller does not count, so
// share unbinds it (machine-level with --pool), and says so; if that fails,
// it says the old one is live WITHOUT admission.
func TestShareAdmissionUnbindsAKeylessBindingOnFailure(t *testing.T) {
	failing := func() *fakeRunner {
		return &fakeRunner{on: func(line string) (Result, bool) {
			if strings.HasPrefix(line, "zm-run-conformance ") {
				return Result{Exit: 1}, true
			}
			return Result{}, true
		}}
	}
	o := admissionOptions()
	o.Sizes = []string{"c2m4"}
	o.AdmissionURL = "https://ci.example"
	o.Earlier = []computeapi.RunTarget{{Name: "h-c2m4", Kind: "box", Config: map[string]string{"vm": "oci"}}}
	u := &unbindRecorder{}
	res, err := shareOn(context.Background(), failing(), u, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.project) != 1 || u.project[0] != "h-c2m4" || len(u.host) != 0 || !strings.Contains(res.Sizes[0].Error, "had no admission keys, so it was unbound") {
		t.Errorf("unbound %v %v: %q", u.project, u.host, res.Sizes[0].Error)
	}
	o.Pool = "team"
	u = &unbindRecorder{}
	if _, err := shareOn(context.Background(), failing(), u, o); err != nil || len(u.host) != 1 {
		t.Errorf("--pool: %v %v", err, u.host)
	}
	u = &unbindRecorder{err: errors.New("it has live handles")}
	if res, _ = shareOn(context.Background(), failing(), u, o); !strings.Contains(res.Sizes[0].Error,
		"h-c2m4 is still bound from before WITHOUT admission: its boxes are uncounted by the controller (unbind: it has live handles); run again, or zm target unbind --host h-c2m4") {
		t.Errorf("unbind failed: %q", res.Sizes[0].Error)
	}
	// An earlier binding with admission keys, or a run without them: nothing is unbound.
	o.Earlier[0].Config["admission_url"] = "https://old.example"
	u = &unbindRecorder{}
	if _, err := shareOn(context.Background(), failing(), u, o); err != nil || len(u.host)+len(u.project) != 0 {
		t.Errorf("keyed earlier: %v %v %v", err, u.host, u.project)
	}
	o.AdmissionURL, o.Earlier[0].Config = NoAdmission, map[string]string{"vm": "oci"}
	if _, err := shareOn(context.Background(), failing(), u, o); err != nil || len(u.host)+len(u.project) != 0 {
		t.Errorf("none: %v %v %v", err, u.host, u.project)
	}
}
