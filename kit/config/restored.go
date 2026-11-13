package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FileRestored names the home's restore generation (SPEC 17.9 "Off-site
// home backups"): a random id `zm backup restore` writes before it moves a
// set into place. Never in a set (a set holds only what offsite.HomeFile
// allows), so every restore mints a new one, and an ordinary restart keeps
// it. A team authority sends it with each snapshot; a replica that took its
// copy under another one is told to take a snapshot again, which drops what
// the restored home lost (zm-c5y6c.22.2).
const FileRestored = "restored"

// NewRestoreGeneration is a fresh restore generation.
func NewRestoreGeneration() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// WriteRestoreGeneration writes gen into home and syncs the file and its
// directory, before anything restored is moved into place.
func WriteRestoreGeneration(home, gen string) error {
	path := filepath.Join(home, FileRestored)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(gen + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	d, err := os.Open(home)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// RestoreGeneration reads home's restore generation: "" when the home was
// never restored. A file that is there but cannot be read, or holds no
// generation, is an error: a caller must not take it for "never restored".
func RestoreGeneration(home string) (string, error) {
	b, err := os.ReadFile(filepath.Join(home, FileRestored))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	gen := strings.TrimSpace(string(b))
	if len(gen) != 32 {
		return "", fmt.Errorf("%s: not a restore generation", FileRestored)
	}
	if _, err := hex.DecodeString(gen); err != nil {
		return "", fmt.Errorf("%s: not a restore generation", FileRestored)
	}
	return gen, nil
}

// FileRestoring marks a home whose restore is moving files in (zm-c5y6c.22.3).
// `zm backup restore` writes it, holding the set stamp, before any file or
// the restore generation lands, and removes it only after every file and
// directory is synced. Never in a set. A daemon refuses a home that has it
// (CheckNotRestoring): what is there may be partial.
const FileRestoring = "restoring"

// MarkRestoring durably writes the restoring marker for set stamp into home.
func MarkRestoring(home, stamp string) error {
	path := filepath.Join(home, FileRestoring)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(stamp + "\n"); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(home)
}

// ClearRestoring removes the restoring marker and syncs home.
func ClearRestoring(home string) error {
	if err := os.Remove(filepath.Join(home, FileRestoring)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(home)
}

// CheckNotRestoring is nil when home holds no restoring marker. A marker, or
// one that cannot be read, is an error: the home may be partial.
func CheckNotRestoring(home string) error {
	b, err := os.ReadFile(filepath.Join(home, FileRestoring))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s holds a %s marker that cannot be read (%v): it may be a partial restore. Restore again into a fresh home (zm backup restore --to <new dir>)", home, FileRestoring, err)
	}
	stamp := strings.TrimSpace(string(b))
	if stamp == "" || strings.ContainsAny(stamp, " \t\r\n") {
		return fmt.Errorf("%s holds a malformed %s marker: it may be a partial restore. Restore again into a fresh home (zm backup restore --to <new dir>)", home, FileRestoring)
	}
	return fmt.Errorf("%s holds an interrupted restore of set %s: it may be partial. Restore again into a fresh home (zm backup restore --to <new dir>)", home, stamp)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
