package boxcp

import (
	"path"
	"strings"
)

// A copy is not a secret channel: it writes to the box's disk and crosses
// every leg as plain data. Upload and WriteFile name what they wrote whose
// path looks like a credentials file (Result.CredentialLike), so the CLI and
// the MCP tool can say so; they refuse nothing. Secrets go through exec's
// secrets or the box's secrets verb (docs/daemon.md, Secret grants).

// credentialNames are base names, lower case, that hold credentials.
var credentialNames = map[string]bool{
	".env": true, ".netrc": true, "_netrc": true, ".npmrc": true, ".pypirc": true, ".pgpass": true,
	".git-credentials": true, ".dockercfg": true, ".htpasswd": true, "kubeconfig": true,
	"credentials": true, "credentials.json": true, "credentials.yml": true, "credentials.yaml": true,
	"client_secret.json": true, "secrets.json": true, "secrets.yml": true, "secrets.yaml": true,
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
}

// credentialExts are extensions, lower case, of key and keystore files.
var credentialExts = map[string]bool{".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true, ".kdbx": true}

// credentialIn are files at a path ending dir/base: tools' login stores.
var credentialIn = map[string]bool{".docker/config.json": true, ".kube/config": true, "gh/hosts.yml": true}

// CredentialLike says whether p (slash-separated) looks like a credentials
// file by its name: a guess for a warning, never a refusal.
func CredentialLike(p string) bool {
	p = strings.ToLower(path.Clean("/" + p))
	base, dir := path.Base(p), path.Base(path.Dir(p))
	switch {
	case credentialNames[base], credentialExts[path.Ext(base)], credentialIn[dir+"/"+base]:
		return true
	case strings.HasPrefix(base, ".env."): // .env.local, .env.production; not the checked-in examples
		for _, s := range []string{".example", ".sample", ".template", ".dist"} {
			if strings.HasSuffix(base, s) {
				return false
			}
		}
		return true
	case strings.HasPrefix(base, "id_") && !strings.HasSuffix(base, ".pub"): // an SSH key, also from a copied .ssh itself
		return true
	case dir == ".ssh": // keys; not what is public
		return !strings.HasSuffix(base, ".pub") && base != "known_hosts" && base != "config" && base != "authorized_keys"
	case dir == ".gnupg":
		return true
	}
	return false
}
