package secrets_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/secrets"
)

func TestValueMasks(t *testing.T) {
	v := secrets.NewValue("hunter2-hunter2")
	j, _ := json.Marshal(struct{ K secrets.Value }{v})
	hidden := struct{ k secrets.Value }{v} // fmt skips methods on unexported fields
	for _, s := range []string{fmt.Sprint(v), fmt.Sprintf("%v %s %q %+v %#v %x", v, v, v, v, v, v), fmt.Sprintf("%v", &v), string(j),
		fmt.Sprintf("%v %+v %#v", hidden, hidden, hidden), fmt.Sprintf("%+v", struct{ K secrets.Value }{v})} {
		if strings.Contains(s, "hunter2") {
			t.Fatalf("leaked: %s", s)
		}
	}
	if v.Reveal() != "hunter2-hunter2" {
		t.Fatal("Reveal")
	}
	if json.Unmarshal([]byte(`{"K":"x"}`), &struct{ K secrets.Value }{}) == nil {
		t.Fatal("a Value decoded")
	}
}

func TestFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "key")
	os.WriteFile(p, []byte("  lin_api_x \n"), 0o600)
	v, err := secrets.Get(ctx, secrets.File(p))
	if err != nil || v.Reveal() != "lin_api_x" {
		t.Fatalf("%q %v", v.Reveal(), err)
	}
	os.Chmod(p, 0o644)
	_, err = secrets.Get(ctx, secrets.File(p))
	if err == nil || !strings.Contains(err.Error(), "readable or writable by others") {
		t.Fatalf("a 0644 file: %v", err)
	}
	if _, err := secrets.Get(ctx, secrets.File(filepath.Join(dir, "none"))); !errors.Is(err, secrets.ErrNotFound) || !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := secrets.Get(ctx, secrets.File("rel/key")); err == nil {
		t.Fatal("a relative path read")
	}
	if _, err := secrets.Get(ctx, secrets.File(dir)); err == nil {
		t.Fatal("a directory read")
	}
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, nil, 0o600)
	if v, err := secrets.Get(ctx, secrets.File(empty)); err != nil || !v.IsZero() {
		t.Fatalf("empty: %v", err)
	}
	big := filepath.Join(dir, "big")
	os.WriteFile(big, make([]byte, secrets.MaxFile+1), 0o600)
	if _, err := secrets.Get(ctx, secrets.File(big)); err == nil {
		t.Fatal("an oversized file read")
	}
}

func TestEnvAndParse(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ZM_TEST_SECRET", "s3")
	if v, err := secrets.Get(ctx, secrets.Env("ZM_TEST_SECRET")); err != nil || v.Reveal() != "s3" {
		t.Fatal(err)
	}
	if _, err := secrets.Get(ctx, secrets.Env("ZM_TEST_SECRET_UNSET_X")); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	for in, want := range map[string]secrets.Ref{
		"file:~/k":           {Scheme: "file", Name: home + "/k"},
		"keychain:zm-linear": {Scheme: "keychain", Name: "zm-linear"},
		"keychain:svc:acct":  {Scheme: "keychain", Name: "svc", Account: "acct"},
		"env:X":              {Scheme: "env", Name: "X"},
	} {
		if r, err := secrets.Parse(in); err != nil || r != want {
			t.Errorf("%s: %+v %v", in, r, err)
		}
	}
	for _, bad := range []string{"", "file", "file:", "op:x", "nokey"} {
		if _, err := secrets.Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := (&secrets.Reader{}).Get(ctx, secrets.Env("X")); err == nil {
		t.Fatal("the zero Reader read")
	}
}

func TestKeychainFakeBin(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bin := filepath.Join(dir, "security")
	// exit 44 = no such item; otherwise print the service name as the secret.
	os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$3\" = missing ] && exit 44\n[ \"$3\" = locked ] && { echo 'secret-in-stdout'; exit 51; }\necho \"pw-$3-$6\"\n"), 0o700)
	r := secrets.Default().With("keychain", secrets.KeychainDriver{Bin: bin})
	if v, err := r.Get(ctx, secrets.Keychain("svc", "acct")); err != nil || v.Reveal() != "pw-svc-acct" {
		t.Fatalf("%q %v", v.Reveal(), err)
	}
	if _, err := r.Get(ctx, secrets.Keychain("missing", "")); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	_, err := r.Get(ctx, secrets.Keychain("locked", ""))
	if err == nil || errors.Is(err, secrets.ErrNotFound) || strings.Contains(err.Error(), "secret-in-stdout") {
		t.Fatalf("locked: %v", err)
	}
}

// ReadFile reads a missing file as empty, with no error.
func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	if v, err := secrets.ReadFile(filepath.Join(dir, "missing")); err != nil || v != "" {
		t.Fatalf("missing: %q %v", v, err)
	}
	p := filepath.Join(dir, "pw")
	if err := os.WriteFile(p, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := secrets.ReadFile(p); err != nil || v != "hunter2" {
		t.Fatalf("read: %q %v", v, err)
	}
}

// CopyFile stores a password 0600 in a 0700 directory, and refuses a file
// that cannot be one.
func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "pw")
	if err := os.WriteFile(from, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(dir, "secrets", "proj", "bd.password")
	if err := secrets.CopyFile(from, to); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(to); err != nil || string(b) != "hunter2\n" {
		t.Fatalf("copy: %q %v", b, err)
	}
	for path, want := range map[string]os.FileMode{to: 0o600, filepath.Dir(to): 0o700, filepath.Dir(filepath.Dir(to)): 0o700} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %o", path, st, err, want)
		}
	}
	for name, content := range map[string]string{"empty": "", "big": strings.Repeat("x", 4097)} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := secrets.CopyFile(p, to); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := secrets.CopyFile(filepath.Join(dir, "missing"), to); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("missing: %v", err)
	}
}

// Invalid input may be a pasted token rather than a reference. Neither Parse
// nor the API error wrappers may echo any part of that untrusted input.
func TestParseErrorsDoNotExposeInvalidSecretInput(t *testing.T) {
	const canary = "SYNTHETIC-SECRET-CANARY"
	for name, input := range map[string]string{
		"bare": canary, "empty name": canary + ":", "unknown scheme": canary + ":synthetic-tail",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := secrets.Parse(input)
			if !api.IsCode(err, api.CodeBadRequest) {
				t.Fatalf("wanted bad_request, got %v", err)
			}
			for _, wrapped := range []error{err, secrets.Prefix(err, "watchdog: "), secrets.Recode(err, api.CodeUnavailable)} {
				if strings.Contains(wrapped.Error(), canary) || strings.Contains(wrapped.Error(), "synthetic-tail") {
					t.Fatal("parse diagnostic exposed invalid input")
				}
				rec := httptest.NewRecorder()
				api.WriteError(rec, wrapped)
				if strings.Contains(rec.Body.String(), canary) || strings.Contains(rec.Body.String(), "synthetic-tail") {
					t.Fatal("API response exposed invalid input")
				}
				if !strings.Contains(rec.Body.String(), "file:") || !strings.Contains(rec.Body.String(), "env:") {
					t.Fatal("supported reference hints were lost")
				}
			}
		})
	}
}
