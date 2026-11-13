package boxcp_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sengirunners/potluck/boxcp"
)

func TestCredentialLike(t *testing.T) {
	for p, want := range map[string]bool{
		".env": true, "app/.env.local": true, "app/.env.example": false, ".netrc": true, "home/.aws/credentials": true,
		".ssh/id_ed25519": true, ".ssh/id_ed25519.pub": false, ".ssh/known_hosts": false, "certs/server.KEY": true,
		"tls/ca.pem": true, ".docker/config.json": true, ".kube/config": true, ".config/gh/hosts.yml": true,
		"config.json": false, "src/main.go": false, "README.md": false, "keys/": false, ".gnupg/pubring.kbx": true,
		"notes/environment.txt": false, "id_work": true, "id_work.pub": false,
	} {
		if got := boxcp.CredentialLike(p); got != want {
			t.Errorf("CredentialLike(%q) = %v, want %v", p, got, want)
		}
	}
}

// Upload and WriteFile name what they wrote that looks like a credentials
// file, and still write it: a warning, never a refusal.
func TestCopyNamesCredentialLikeFiles(t *testing.T) {
	ctx := context.Background()
	exec, sh := refBox(t)
	src := filepath.Join(t.TempDir(), "proj")
	write(t, filepath.Join(src, "main.go"), []byte("package main\n"), 0o644)
	write(t, filepath.Join(src, ".env"), []byte("TOKEN=x\n"), 0o600)
	write(t, filepath.Join(src, "deploy", "id_rsa"), []byte("k"), 0o600)
	res, err := boxcp.Upload(ctx, exec, src, "work", boxcp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(res.CredentialLike)
	if want := []string{".env", "deploy/id_rsa"}; !slices.Equal(res.CredentialLike, want) || sh(`cat work/.env`).Stdout != "TOKEN=x\n" {
		t.Errorf("a directory: credential_like %q, want %q, and the file copied", res.CredentialLike, want)
	}
	f := filepath.Join(src, ".env")
	if res, err := boxcp.Upload(ctx, exec, f, "one", boxcp.Options{}); err != nil || !slices.Equal(res.CredentialLike, []string{f}) {
		t.Errorf("a file: %q %v, want it named", res.CredentialLike, err)
	}
	if res, err := boxcp.Upload(ctx, exec, filepath.Join(src, "main.go"), "two", boxcp.Options{}); err != nil || res.CredentialLike != nil {
		t.Errorf("a plain file: %q %v, want nothing named", res.CredentialLike, err)
	}
	if res, err := boxcp.WriteFile(ctx, exec, "cfg/.netrc", []byte("machine x"), boxcp.Options{}); err != nil || !slices.Equal(res.CredentialLike, []string{"cfg/.netrc"}) {
		t.Errorf("write_file: %q %v, want it named", res.CredentialLike, err)
	}
}
