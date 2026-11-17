package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// verifySSHSIG checks an armored SSHSIG signature of msg the way OpenSSH's
// PROTOCOL.sshsig defines it, independently of SSHSign's code: the blob's
// fields in order, then the Ed25519 signature over the signed data.
func verifySSHSIG(armored []byte, pub ed25519.PublicKey, namespace string, msg []byte) error {
	s := strings.TrimSpace(string(armored))
	body, ok := strings.CutPrefix(s, "-----BEGIN SSH SIGNATURE-----\n")
	if !ok {
		return errors.New("no BEGIN line")
	}
	body, ok = strings.CutSuffix(body, "\n-----END SSH SIGNATURE-----")
	if !ok {
		return errors.New("no END line")
	}
	for _, l := range strings.Split(body, "\n") {
		if len(l) > 76 {
			return fmt.Errorf("armor line of %d characters", len(l))
		}
	}
	blob, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body, "\n", ""))
	if err != nil {
		return err
	}
	r := bytes.NewReader(blob)
	magic := make([]byte, 6)
	if _, err := r.Read(magic); err != nil || string(magic) != "SSHSIG" {
		return errors.New("no SSHSIG magic")
	}
	var version uint32
	if err := binary.Read(r, binary.BigEndian, &version); err != nil || version != 1 {
		return fmt.Errorf("version %d", version)
	}
	str := func() []byte {
		var n uint32
		if binary.Read(r, binary.BigEndian, &n) != nil || int(n) > r.Len() {
			return nil
		}
		b := make([]byte, n)
		_, _ = r.Read(b)
		return b
	}
	pk, ns, reserved, hash, sig := str(), str(), str(), str(), str()
	if r.Len() != 0 {
		return errors.New("trailing bytes")
	}
	if !bytes.Equal(pk, SSHPublicKey(pub)) {
		return errors.New("another public key")
	}
	if string(ns) != namespace || len(reserved) != 0 || string(hash) != "sha512" {
		return fmt.Errorf("namespace %q, reserved %q, hash %q", ns, reserved, hash)
	}
	sr := bytes.NewReader(sig)
	r = sr
	alg, raw := str(), str()
	if string(alg) != "ssh-ed25519" || len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("signature algorithm %q", alg)
	}
	h := sha512.Sum512(msg)
	var signed bytes.Buffer
	signed.WriteString("SSHSIG")
	sshString(&signed, []byte(namespace))
	sshString(&signed, nil)
	sshString(&signed, []byte("sha512"))
	sshString(&signed, h[:])
	if !ed25519.Verify(pub, signed.Bytes(), raw) {
		return errors.New("signature does not verify")
	}
	return nil
}

// The SSHSIG signature is the key's over the exact message, bound to the
// channel's namespace: another message, key or channel never verifies.
func TestSSHSign(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	msg := []byte("{\"schema\": \"potluck-release/1\"}\n")
	sig := Potluck.SSHSign(key, msg)
	if err := verifySSHSIG(sig, pub, "potluck-release", msg); err != nil {
		t.Fatalf("a good signature: %v\n%s", err, sig)
	}
	if verifySSHSIG(sig, pub, "potluck-release", append(append([]byte(nil), msg...), ' ')) == nil {
		t.Error("verified over an altered message")
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if verifySSHSIG(sig, other, "potluck-release", msg) == nil {
		t.Error("verified with another key")
	}
	if verifySSHSIG(sig, pub, ZM.Namespace, msg) == nil {
		t.Error("a Potluck signature verified as zm's")
	}
	if Potluck.Namespace == ZM.Namespace || Potluck.Namespace == "" || ZM.Namespace == "" {
		t.Errorf("namespaces %q and %q must differ and be set", Potluck.Namespace, ZM.Namespace)
	}
	if got, want := Potluck.AllowedSigner("potluck-release", pub), `potluck-release namespaces="potluck-release" ssh-ed25519 `+
		base64.StdEncoding.EncodeToString(SSHPublicKey(pub)); got != want {
		t.Errorf("AllowedSigner = %q, want %q", got, want)
	}
}

// OpenSSH itself accepts the signature for the allowed signer and namespace
// only (skipped where there is no ssh-keygen).
func TestSSHSignVerifiesWithSSHKeygen(t *testing.T) {
	kg, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen here")
	}
	pub, key, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	msg := []byte("{\"schema\": \"potluck-release/1\"}\n")
	sig, allowed := filepath.Join(dir, "m.sshsig"), filepath.Join(dir, "allowed")
	if err := os.WriteFile(sig, Potluck.SSHSign(key, msg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(allowed, []byte(Potluck.AllowedSigner("potluck-release", pub)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	verify := func(ns string, body []byte) error {
		c := exec.Command(kg, "-Y", "verify", "-f", allowed, "-I", "potluck-release", "-n", ns, "-s", sig)
		c.Stdin = bytes.NewReader(body)
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	if err := verify("potluck-release", msg); err != nil {
		t.Fatalf("ssh-keygen refused a good signature: %v", err)
	}
	if verify("potluck-release", append(append([]byte(nil), msg...), ' ')) == nil {
		t.Error("ssh-keygen accepted an altered message")
	}
	if verify("file", msg) == nil {
		t.Error("ssh-keygen accepted the signature under another namespace")
	}
}

// SignDir writes both signatures, each over the manifest's exact bytes.
func TestSignDirWritesTheSSHSIG(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	name := Potluck.Tarball("main-abc", "linux", "amd64")
	sum := sha256.Sum256([]byte("tarball"))
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Potluck.SignDir(dir, "main-abc", commit, time.Now(), key); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, ManifestFile))
	sshsig, err := os.ReadFile(filepath.Join(dir, SSHSIGFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySSHSIG(sshsig, pub, Potluck.Namespace, body); err != nil {
		t.Fatalf("manifest.json.sshsig: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, SignatureFile))
	if _, err := Potluck.Verify(body, string(raw), []string{FormatKey(pub)}); err != nil {
		t.Fatalf("manifest.json.sig: %v", err)
	}
}
