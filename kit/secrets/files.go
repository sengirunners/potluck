package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/sengirunners/potluck/kit/api"
)

// ReadFile is the secret in the file at path, revealed. A missing file reads
// as "" with no error (a binding without a stored password).
func ReadFile(path string) (string, error) {
	v, err := Get(context.Background(), File(path))
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return v.Reveal(), err
}

// CopyFile copies a password file into $ZM_HOME/secrets (0600, dir 0700). It
// refuses an empty file or one over 4 KiB.
func CopyFile(from, to string) error {
	v, err := Get(context.Background(), File(from))
	if err != nil {
		return Recode(err, api.CodeBadRequest)
	}
	if v.IsZero() || len(v.Reveal()) > 4096 {
		return api.Errorf(api.CodeBadRequest, "password file %s: want a small, non-empty file", from)
	}
	if err := WriteFile(to, v.Reveal()+"\n"); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Dir(filepath.Dir(to)), 0o700)
	return nil
}

// WriteFile stores v in the secret file at path (0600, its directory 0700)
// through a temporary file and a rename, so a reader never sees half of it.
func WriteFile(path, v string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(v), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
