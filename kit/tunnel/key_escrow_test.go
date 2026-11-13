package tunnel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/secrets"
)

// serviceAt is a tunnel service on home with tunnel.key = escrow.
func serviceAt(t *testing.T, home, escrow string) *Service {
	t.Helper()
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.File.Tunnel.Key = escrow
	s := New(&core.Deps{Config: cfg})
	s.Secrets = secrets.Default()
	return s
}

func tempHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return home
}

func postKey(t *testing.T, s *Service, ref string) (int, api.TunnelStatus, string) {
	t.Helper()
	b, _ := json.Marshal(api.TunnelKeyRequest{Ref: ref})
	req := httptest.NewRequest(http.MethodPost, "/v0/tunnel/key", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleKey(rec, req)
	var st api.TunnelStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	return rec.Code, st, rec.Body.String()
}

// zm-c5y6c.20: the home key's own path across a restore.
func TestKeyEscrow(t *testing.T) {
	home := tempHome(t)
	s := serviceAt(t, home, "")
	fp, err := s.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := os.ReadFile(filepath.Join(home, KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	escrow := filepath.Join(t.TempDir(), "home-key")
	other := filepath.Join(t.TempDir(), "other-key")
	os.WriteFile(escrow, seed, 0o600)
	os.WriteFile(other, []byte(strings.Repeat("ab", 32)+"\n"), 0o600)

	// The route saves an escrow only when it holds this key.
	if code, _, body := postKey(t, s, "file:"+other); code != http.StatusConflict {
		t.Fatalf("an escrow with another key: %d %s", code, body)
	}
	if code, _, body := postKey(t, s, "file:"+filepath.Join(t.TempDir(), "missing")); code/100 == 2 {
		t.Fatalf("a missing escrow: %d %s", code, body)
	}
	if code, _, body := postKey(t, s, "nope:x"); code != http.StatusBadRequest {
		t.Fatalf("a bad reference: %d %s", code, body)
	}
	if c, _ := config.LoadFrom(home); c.File.Tunnel.Key != "" {
		t.Fatalf("a refused escrow was saved: %q", c.File.Tunnel.Key)
	}
	code, st, body := postKey(t, s, "file:"+escrow)
	if code != 200 || st.KeyEscrow != "file:"+escrow || st.Fingerprint != fp {
		t.Fatalf("saving the escrow: %d %s", code, body)
	}
	if c, _ := config.LoadFrom(home); c.File.Tunnel.Key != "file:"+escrow {
		t.Fatalf("config after saving: %q", c.File.Tunnel.Key)
	}
	// tunnel down keeps it: the key is the daemon's, not the listener's.
	rec := httptest.NewRecorder()
	s.handleDown(rec, httptest.NewRequest(http.MethodPost, "/v0/tunnel/down", nil))
	if c, _ := config.LoadFrom(home); c.File.Tunnel.Key != "file:"+escrow {
		t.Fatalf("tunnel down dropped the escrow: %q", c.File.Tunnel.Key)
	}

	// A restored home: no key file, the escrow named. Same key, not fresh.
	restored := tempHome(t)
	r := serviceAt(t, restored, "file:"+escrow)
	if got, err := r.Fingerprint(); err != nil || got != fp {
		t.Fatalf("restored key %q %v, want %q", got, err, fp)
	}
	if r.keyFresh {
		t.Fatal("a key from the escrow counted as fresh: the replay hold would be skipped")
	}
	if b, err := os.ReadFile(filepath.Join(restored, KeyFile)); err != nil || string(b) != string(seed) {
		t.Fatalf("the escrowed key was not written to the home: %v", err)
	}

	// An escrow that cannot be read: no new key, ever.
	lost := tempHome(t)
	l := serviceAt(t, lost, "file:"+filepath.Join(t.TempDir(), "gone"))
	if got, err := l.Fingerprint(); err == nil {
		t.Fatalf("a home whose escrow is gone made key %q", got)
	}
	if _, err := os.Stat(filepath.Join(lost, KeyFile)); !os.IsNotExist(err) {
		t.Fatalf("a home whose escrow is gone wrote a key file: %v", err)
	}
	if st := l.Status(); !strings.Contains(st.Error, "no home key") || !strings.Contains(st.Error, "adopts") {
		t.Fatalf("status does not say there is no key and how to recover: %+v", st)
	}
	// Clearing the escrow of a daemon with no key would make a new key.
	if code, _, body := postKey(t, l, ""); code != http.StatusConflict {
		t.Fatalf("clearing with no key: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(lost, KeyFile)); !os.IsNotExist(err) {
		t.Fatalf("a refused clear wrote a key: %v", err)
	}
	// Recovery: point it at where the key is now; it adopts it, not fresh.
	if code, _, body := postKey(t, l, "file:"+other); code != 200 {
		t.Fatalf("adopting the key from a moved escrow: %d %s", code, body)
	}
	if l.keyFresh {
		t.Fatal("an adopted key counted as fresh")
	}
	if got, err := l.Fingerprint(); err != nil || got == fp {
		t.Fatalf("adopted %q %v (want the other file's key)", got, err)
	}
	if code, _, body := postKey(t, l, "file:"+escrow); code != http.StatusConflict {
		t.Fatalf("a second, different key after adopting: %d %s", code, body)
	}
	// A file inside the home, or a relative path, is no escrow.
	if code, _, body := postKey(t, s, "file:"+filepath.Join(home, KeyFile)); code != http.StatusBadRequest {
		t.Fatalf("the home's own key file as escrow: %d %s", code, body)
	}
	for _, spell := range homeSpellings(home) {
		if code, _, body := postKey(t, s, "file:"+filepath.Join(spell, KeyFile)); code != http.StatusBadRequest {
			t.Fatalf("the home's own key file spelled %s: %d %s", spell, code, body)
		}
	}
	if InHome(home, home+"4") || InHome(home, filepath.Join(filepath.Dir(home), "elsewhere")) {
		t.Fatal("a sibling of the home counted as inside it")
	}
	if !InHome(home, filepath.Join(home, "not", "yet", "made")) {
		t.Fatal("a path under the home whose folders do not exist yet counted as outside it")
	}
	if code, _, body := postKey(t, s, "file:tunnel/key"); code != http.StatusBadRequest {
		t.Fatalf("a relative escrow path: %d %s", code, body)
	}
	// Clearing with a key loaded is fine.
	if code, st, body := postKey(t, s, ""); code != 200 || st.KeyEscrow != "" {
		t.Fatalf("clearing: %d %s", code, body)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(bad, []byte("not a seed"), 0o600)
	if _, err := serviceAt(t, tempHome(t), "file:"+bad).Fingerprint(); err == nil {
		t.Fatal("an escrow holding no seed gave a key")
	}

	// A corrupt key file is never written over, and says what to do.
	corrupt := tempHome(t)
	os.MkdirAll(filepath.Join(corrupt, "tunnel"), 0o700)
	os.WriteFile(filepath.Join(corrupt, KeyFile), []byte("garbage"), 0o600)
	if code, _, body := postKey(t, serviceAt(t, corrupt, "file:"+escrow), "file:"+escrow); code != http.StatusConflict || !strings.Contains(body, "aside") {
		t.Fatalf("adopting over a corrupt key file: %d %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(corrupt, KeyFile)); string(b) != "garbage" {
		t.Fatal("a corrupt key file was written over")
	}

	// A key file wins over the escrow (an ordinary restart reads no secret).
	both := tempHome(t)
	os.MkdirAll(filepath.Join(both, "tunnel"), 0o700)
	os.WriteFile(filepath.Join(both, KeyFile), seed, 0o600)
	if got, err := serviceAt(t, both, "file:"+other).Fingerprint(); err != nil || got != fp {
		t.Fatalf("key file vs escrow: %q %v", got, err)
	}
}

// homeSpellings is home as macOS also spells it: another case (the
// default volume is case-insensitive) and through the Data firmlink.
func homeSpellings(home string) []string {
	out := []string{home + "/"}
	if r, err := filepath.EvalSymlinks(home); err == nil {
		out = append(out, r)
		up := filepath.Join(filepath.Dir(r), strings.ToUpper(filepath.Base(r)))
		if _, err := os.Stat(up); err == nil {
			out = append(out, up)
		}
		if fl := "/System/Volumes/Data" + r; func() bool { _, err := os.Stat(fl); return err == nil }() {
			out = append(out, fl)
		}
	}
	return out
}
