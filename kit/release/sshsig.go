package release

// SSHSIG (OpenSSH's PROTOCOL.sshsig): the release key also signs the
// manifest in the format `ssh-keygen -Y verify` reads. A stock Mac cannot
// check the raw Ed25519 signature (its openssl is LibreSSL, which has no
// Ed25519), but every Mac and most Linux machines ship OpenSSH's
// ssh-keygen, so install.sh checks this one. Both signatures are the same
// key's over the same bytes.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"strings"
)

// SSHSIGFile is the release file holding the manifest's SSHSIG signature.
const SSHSIGFile = "manifest.json.sshsig"

// sshString appends b as an SSH wire string: a uint32 length, then the bytes.
func sshString(buf *bytes.Buffer, b []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	buf.Write(n[:])
	buf.Write(b)
}

// SSHPublicKey is pub in SSH wire format, what an allowed_signers or
// authorized_keys line base64-encodes after "ssh-ed25519".
func SSHPublicKey(pub ed25519.PublicKey) []byte {
	var b bytes.Buffer
	sshString(&b, []byte("ssh-ed25519"))
	sshString(&b, pub)
	return b.Bytes()
}

// AllowedSigner is the allowed_signers line `ssh-keygen -Y verify` takes for
// pub as principal, limited to f's namespace.
func (f Format) AllowedSigner(principal string, pub ed25519.PublicKey) string {
	return principal + ` namespaces="` + f.Namespace + `" ssh-ed25519 ` + base64.StdEncoding.EncodeToString(SSHPublicKey(pub))
}

// SSHSign returns the armored SSHSIG signature of msg under f's namespace
// (hash sha512), as `ssh-keygen -Y sign -n <namespace>` writes it. The
// namespace binds it to this use: a git commit or a file signed with the
// same key never verifies as a release.
func (f Format) SSHSign(key ed25519.PrivateKey, msg []byte) []byte {
	h := sha512.Sum512(msg)
	var signed bytes.Buffer
	signed.WriteString("SSHSIG")
	sshString(&signed, []byte(f.Namespace))
	sshString(&signed, nil) // reserved
	sshString(&signed, []byte("sha512"))
	sshString(&signed, h[:])
	var sig bytes.Buffer
	sshString(&sig, []byte("ssh-ed25519"))
	sshString(&sig, ed25519.Sign(key, signed.Bytes()))

	var blob bytes.Buffer
	blob.WriteString("SSHSIG")
	blob.Write([]byte{0, 0, 0, 1}) // version
	sshString(&blob, SSHPublicKey(key.Public().(ed25519.PublicKey)))
	sshString(&blob, []byte(f.Namespace))
	sshString(&blob, nil)
	sshString(&blob, []byte("sha512"))
	sshString(&blob, sig.Bytes())

	enc := base64.StdEncoding.EncodeToString(blob.Bytes())
	var out strings.Builder
	out.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	for len(enc) > 70 {
		out.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	out.WriteString(enc + "\n-----END SSH SIGNATURE-----\n")
	return []byte(out.String())
}
