package secrets_test

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/secrets"
)

// fakeSecurityEnv names the item directory when the test binary runs as a
// stand-in for /usr/bin/security (see fakeSecurity).
const fakeSecurityEnv = "POTLUCK_TEST_FAKE_SECURITY_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeSecurityEnv); dir != "" {
		os.Exit(fakeSecurity(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeSecurity is the part of /usr/bin/security the keychain driver uses,
// keeping items as files in dir: find-generic-password -w (exit 44 when
// missing), delete-generic-password, and -i reading one add-generic-password
// line from stdin (-X hex value, -U to update; a duplicate without -U writes
// to stderr and exits 45, as security does). Every argv goes to dir/argv.log.
// A file dir/locked makes every command fail, as a locked keychain does; a
// file dir/garble makes an add store the value short and still succeed.
func fakeSecurity(dir string, args []string) int {
	if f, err := os.OpenFile(filepath.Join(dir, "argv.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	if _, err := os.Stat(filepath.Join(dir, "locked")); err == nil {
		fmt.Fprintln(os.Stderr, "security: The user name or passphrase you entered is not correct.")
		return 51
	}
	if len(args) == 1 && args[0] == "-i" {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		args = strings.Fields(line)
	}
	if len(args) == 0 {
		return 2
	}
	var service, account, hexValue string
	update := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-s":
			i++
			service = args[i]
		case "-a":
			i++
			account = args[i]
		case "-X":
			i++
			hexValue = args[i]
		case "-U":
			update = true
		}
	}
	item := filepath.Join(dir, "item-"+hex.EncodeToString([]byte(service))+"-"+hex.EncodeToString([]byte(account)))
	switch args[0] {
	case "find-generic-password":
		b, err := os.ReadFile(item)
		if err != nil {
			return 44
		}
		fmt.Println(string(b))
		return 0
	case "delete-generic-password":
		if err := os.Remove(item); err != nil {
			return 44
		}
		return 0
	case "add-generic-password":
		b, err := hex.DecodeString(hexValue)
		if err != nil {
			return 2
		}
		if _, err := os.Stat(filepath.Join(dir, "garble")); err == nil {
			b = b[:len(b)-1]
		}
		if update {
			if err := os.WriteFile(item, b, 0o600); err != nil {
				return 1
			}
			return 0
		}
		f, err := os.OpenFile(item, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			// security may echo the command line; the driver must not show it.
			fmt.Fprintln(os.Stderr, "security: add-generic-password -X "+hexValue+": "+string(b)+": The specified item already exists in the keychain.")
			return 45
		}
		defer f.Close()
		if _, err := f.Write(b); err != nil {
			return 1
		}
		return 0
	}
	return 2
}

// fakeKeychain is a keychain driver over fakeSecurity; it never touches a
// real keychain.
func fakeKeychain(t *testing.T) (secrets.KeychainDriver, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv(fakeSecurityEnv, dir)
	return secrets.KeychainDriver{Bin: exe}, dir
}

// noLeak fails if err's text holds any of the values.
func noLeak(t *testing.T, err error, values ...string) {
	t.Helper()
	if err == nil {
		return
	}
	for _, v := range values {
		if strings.Contains(err.Error(), v) {
			t.Fatalf("error leaks the secret: %v", err)
		}
	}
}

// putRules is the Putter contract every storing driver keeps.
func putRules(t *testing.T, r *secrets.Reader, ref secrets.Ref) {
	t.Helper()
	ctx := context.Background()
	const first, second = "first-secret-value", "second-secret-value"
	if err := r.Put(ctx, ref, secrets.NewValue(first)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if v, err := r.Get(ctx, ref); err != nil || v.Reveal() != first {
		t.Fatalf("get after put: %v", err)
	}
	if err := r.Put(ctx, ref, secrets.NewValue(first)); err != nil {
		t.Fatalf("put of the same value: %v", err)
	}
	err := r.Put(ctx, ref, secrets.NewValue(second))
	if !errors.Is(err, secrets.ErrExists) || !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("put over another value: %v", err)
	}
	noLeak(t, err, first, second)
	if !errors.Is(secrets.Prefix(err, "home key: "), secrets.ErrExists) {
		t.Fatal("Prefix lost ErrExists")
	}
	if v, _ := r.Get(ctx, ref); v.Reveal() != first {
		t.Fatal("put wrote over another value")
	}
	if err := r.Replace(ctx, ref, secrets.NewValue(second)); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if v, err := r.Get(ctx, ref); err != nil || v.Reveal() != second {
		t.Fatalf("get after replace: %v", err)
	}
	if err := r.Delete(ctx, ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Get(ctx, ref); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := r.Delete(ctx, ref); err != nil {
		t.Fatalf("delete of a missing secret: %v", err)
	}
	for name, bad := range map[string]string{"empty": "", "padded": " " + first + "\n", "too big": strings.Repeat("x", secrets.MaxFile+1)} {
		for verb, put := range map[string]func(context.Context, secrets.Ref, secrets.Value) error{"put": r.Put, "replace": r.Replace} {
			err := put(ctx, ref, secrets.NewValue(bad))
			if !api.IsCode(err, api.CodeBadRequest) {
				t.Fatalf("%s of a %s value: %v", verb, name, err)
			}
			if bad != "" {
				noLeak(t, err, bad)
			}
		}
	}
	if _, err := r.Get(ctx, ref); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("a refused value was stored: %v", err)
	}
}

func TestFilePut(t *testing.T) {
	dir := t.TempDir()
	ref := secrets.File(filepath.Join(dir, "home", "keys", "home.key"))
	putRules(t, secrets.Default(), ref)

	ctx := context.Background()
	if err := secrets.Put(ctx, ref, secrets.NewValue("stored-value")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{ref.Name: 0o600, filepath.Dir(ref.Name): 0o700} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %o", path, st, err, want)
		}
	}
	if b, _ := os.ReadFile(ref.Name); string(b) != "stored-value\n" {
		t.Fatalf("file holds %q", b)
	}
	// No temporary file is left beside it.
	if ents, _ := os.ReadDir(filepath.Dir(ref.Name)); len(ents) != 1 {
		t.Fatalf("directory holds %d entries", len(ents))
	}

	// A loose file is refused, not written over or adopted.
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chmod(loose, 0o644)
	if err := secrets.Put(ctx, secrets.File(loose), secrets.NewValue("new-value")); err == nil || errors.Is(err, secrets.ErrExists) {
		t.Fatalf("put over a loose file: %v", err)
	}
	if b, _ := os.ReadFile(loose); string(b) != "old" {
		t.Fatal("loose file written over")
	}
	// A directory is not a secret file.
	if err := secrets.Put(ctx, secrets.File(dir), secrets.NewValue("v-v")); err == nil {
		t.Fatal("put over a directory")
	}
	if err := secrets.Delete(ctx, secrets.File(filepath.Join(dir, "home"))); err == nil {
		t.Fatal("deleted a directory")
	}
	// The file holds the value and a newline, and Get reads at most MaxFile.
	big := secrets.File(filepath.Join(dir, "big"))
	if err := secrets.Put(ctx, big, secrets.NewValue(strings.Repeat("b", secrets.MaxFile))); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("put of MaxFile bytes: %v", err)
	}
	if err := secrets.Put(ctx, big, secrets.NewValue(strings.Repeat("b", secrets.MaxFile-1))); err != nil {
		t.Fatal(err)
	}
	if v, err := secrets.Get(ctx, big); err != nil || len(v.Reveal()) != secrets.MaxFile-1 {
		t.Fatalf("read back MaxFile-1 bytes: %v", err)
	}
	if err := secrets.Put(ctx, secrets.Ref{Scheme: "file", Name: "relative/key"}, secrets.NewValue("v-v")); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("relative path: %v", err)
	}
}

// Writers racing on one new file: exactly one value lands, every other Put
// fails with ErrExists, and none writes over the winner.
func TestFilePutRace(t *testing.T) {
	ref := secrets.File(filepath.Join(t.TempDir(), "race.key"))
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = secrets.Put(context.Background(), ref, secrets.NewValue(fmt.Sprintf("value-%02d", i)))
		}()
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner >= 0:
			t.Fatalf("two writers won: %d and %d", winner, i)
		case err == nil:
			winner = i
		case !errors.Is(err, secrets.ErrExists):
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no writer won")
	}
	if v, err := secrets.Get(context.Background(), ref); err != nil || v.Reveal() != fmt.Sprintf("value-%02d", winner) {
		t.Fatalf("stored %v, winner %d", err, winner)
	}
	if ents, _ := os.ReadDir(filepath.Dir(ref.Name)); len(ents) != 1 {
		t.Fatalf("losers left %d entries", len(ents))
	}
}

// Readers during Replace see the old value or the new one, never part of
// either, and no temporary file is left.
func TestFileReplaceAtomic(t *testing.T) {
	ctx := context.Background()
	ref := secrets.File(filepath.Join(t.TempDir(), "rotating.key"))
	a, b := strings.Repeat("a", 32<<10), strings.Repeat("b", 32<<10)
	if err := secrets.Put(ctx, ref, secrets.NewValue(a)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var bad error
	go func() {
		defer close(done)
		for range 200 {
			v, err := secrets.Get(ctx, ref)
			if err != nil || v.Reveal() != a && v.Reveal() != b {
				bad = fmt.Errorf("reader saw a partial or missing value: %v", err)
				return
			}
		}
	}()
	for i := range 200 {
		if err := secrets.Replace(ctx, ref, secrets.NewValue([]string{a, b}[i%2])); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	if bad != nil {
		t.Fatal(bad)
	}
	if ents, _ := os.ReadDir(filepath.Dir(ref.Name)); len(ents) != 1 {
		t.Fatalf("replace left %d entries", len(ents))
	}
}

func TestKeychainPut(t *testing.T) {
	k, dir := fakeKeychain(t)
	r := secrets.Default().With("keychain", k)
	putRules(t, r, secrets.Keychain("zm-home-key", "home-1"))
	for verb, err := range map[string]error{
		"put":     r.Put(context.Background(), secrets.Keychain("no-account", ""), secrets.NewValue("v-v")),
		"replace": r.Replace(context.Background(), secrets.Keychain("no-account", ""), secrets.NewValue("v-v")),
		"delete":  r.Delete(context.Background(), secrets.Keychain("no-account", "")),
	} {
		if !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("%s with no account: %v", verb, err)
		}
	}

	ctx := context.Background()
	const value = "keychain-value-xyz"
	if err := r.Put(ctx, secrets.Keychain("svc", "acct"), secrets.NewValue(value)); err != nil {
		t.Fatal(err)
	}
	// The value never reaches argv, plain or hex-encoded.
	log, err := os.ReadFile(filepath.Join(dir, "argv.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{value, hex.EncodeToString([]byte(value))} {
		if strings.Contains(string(log), v) {
			t.Fatalf("value in argv: %s", log)
		}
	}
	for name, ref := range map[string]secrets.Ref{
		"space": secrets.Keychain("two words", "a"), "quote": secrets.Keychain("svc", `a"b`),
		"flag": secrets.Keychain("-U", "a"), "newline": secrets.Keychain("svc\nadd", "a"),
		"hash": secrets.Keychain("svc#x", "a"), "semicolon": secrets.Keychain("svc", "a;b"),
	} {
		if err := r.Put(ctx, ref, secrets.NewValue(value)); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("%s name: %v", name, err)
		}
	}
	for name, v := range map[string]string{"inner space": "two words", "newline": "line1\nline2", "non-ascii": "café-token",
		"too big": strings.Repeat("k", secrets.MaxKeychain+1)} {
		if v == "two words" {
			// A space inside is printable ASCII and is stored.
			if err := r.Put(ctx, secrets.Keychain("spaced", "a"), secrets.NewValue(v)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got, err := r.Get(ctx, secrets.Keychain("spaced", "a")); err != nil || got.Reveal() != v {
				t.Fatalf("%s read back: %v", name, err)
			}
			continue
		}
		err := r.Put(ctx, secrets.Keychain("odd", "a"), secrets.NewValue(v))
		if !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("%s value: %v", name, err)
		}
		noLeak(t, err, v)
	}
	// A store that does not read back the same fails.
	if err := os.WriteFile(filepath.Join(dir, "garble"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for verb, put := range map[string]func(context.Context, secrets.Ref, secrets.Value) error{"put": r.Put, "replace": r.Replace} {
		err := put(ctx, secrets.Keychain("garbled-"+verb, "a"), secrets.NewValue(value))
		if err == nil || errors.Is(err, secrets.ErrExists) {
			t.Fatalf("%s that stored the wrong value: %v", verb, err)
		}
		noLeak(t, err, value)
	}
	os.Remove(filepath.Join(dir, "garble"))
	// A locked keychain fails without prompting, and is not "missing".
	if err := os.WriteFile(filepath.Join(dir, "locked"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for verb, err := range map[string]error{
		"put":     r.Put(ctx, secrets.Keychain("new", "a"), secrets.NewValue(value)),
		"replace": r.Replace(ctx, secrets.Keychain("new", "a"), secrets.NewValue(value)),
		"delete":  r.Delete(ctx, secrets.Keychain("svc", "acct")),
	} {
		if err == nil || errors.Is(err, secrets.ErrNotFound) || errors.Is(err, secrets.ErrExists) {
			t.Fatalf("%s on a locked keychain: %v", verb, err)
		}
		noLeak(t, err, value)
	}
}

// A keychain Put that loses a race (the item appears between the look and
// the add) compares again instead of failing or writing over it.
func TestKeychainPutRace(t *testing.T) {
	k, _ := fakeKeychain(t)
	ref := secrets.Keychain("race", "a")
	ctx := context.Background()
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = k.Put(ctx, ref, secrets.NewValue(fmt.Sprintf("value-%02d", i)))
		}()
	}
	wg.Wait()
	v, err := k.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	for i, err := range errs {
		mine := v.Reveal() == fmt.Sprintf("value-%02d", i)
		if mine && err != nil || !mine && !errors.Is(err, secrets.ErrExists) {
			t.Fatalf("writer %d (stored %v): %v", i, mine, err)
		}
	}
}

func TestEnvRefusesPut(t *testing.T) {
	t.Setenv("POTLUCK_TEST_ENV_SECRET", "kept")
	ctx := context.Background()
	ref := secrets.Env("POTLUCK_TEST_ENV_SECRET")
	for verb, err := range map[string]error{
		"put":     secrets.Put(ctx, ref, secrets.NewValue("new-value")),
		"replace": secrets.Replace(ctx, ref, secrets.NewValue("new-value")),
		"delete":  secrets.Delete(ctx, ref),
	} {
		if !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("%s: %v", verb, err)
		}
		noLeak(t, err, "new-value", "kept")
	}
	if os.Getenv("POTLUCK_TEST_ENV_SECRET") != "kept" {
		t.Fatal("env changed")
	}
}

// A driver with only Get cannot store; one with no Info reports nothing.
func TestPutNeedsAPutter(t *testing.T) {
	ctx := context.Background()
	r := secrets.Default().With("op", secrets.DriverFunc(func(context.Context, secrets.Ref) (secrets.Value, error) {
		return secrets.Value{}, nil
	}))
	if err := r.Put(ctx, secrets.Ref{Scheme: "op", Name: "x"}, secrets.NewValue("v-v")); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("put: %v", err)
	}
	if _, ok := r.Info("op"); ok {
		t.Fatal("a DriverFunc reported Info")
	}
	if err := r.Put(ctx, secrets.Ref{Scheme: "nope", Name: "x"}, secrets.NewValue("v-v")); err == nil {
		t.Fatal("put with no driver")
	}
	if err := r.Put(ctx, secrets.Ref{Scheme: "file"}, secrets.NewValue("v-v")); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("put to nothing: %v", err)
	}
}

// Each built-in driver reports its inventory class and whether it works
// headless.
func TestDriverInfo(t *testing.T) {
	r := secrets.Default()
	for scheme, want := range map[string]secrets.Info{
		"file":     {Class: secrets.ClassO, Headless: true},
		"keychain": {Class: secrets.ClassO, Headless: false},
		"env":      {Class: secrets.ClassO, Headless: true},
	} {
		if got, ok := r.Info(scheme); !ok || got != want {
			t.Fatalf("%s: %+v %v, want %+v", scheme, got, ok, want)
		}
	}
	if _, ok := (*secrets.Reader)(nil).Info("file"); ok {
		t.Fatal("nil Reader reported Info")
	}
}
