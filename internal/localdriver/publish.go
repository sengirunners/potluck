package localdriver

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sengirunners/potluck/protocol"
)

// A run's publish file (docs/protocol.md, A run's publish). At acquire the
// driver reads ZM_PUBLISH_FILE from its environment: a path inside the run's
// checkout. The script is told the file's absolute path in its own
// ZM_PUBLISH_FILE, and what it wrote there is read once the script has ended,
// before the checkout is removed, and kept in the record.

// publishFileFromEnv is the publish file this acquire's environment names:
// "" when unset, a refusal when it is not a clean path inside the checkout.
func publishFileFromEnv() (string, error) {
	p := os.Getenv(protocol.PublishFileEnv)
	if p != "" && !protocol.PublishPathOK(p) {
		return "", refuse("bad_request", "%s must be a clean path inside the run's working directory", protocol.PublishFileEnv)
	}
	return p, nil
}

// runEnv is the script's environment: the allowlist, with ZM_PUBLISH_FILE as
// the publish file's absolute path when the run has one, and absent when not
// (the driver's own value is a path relative to the checkout).
func runEnv(r record) []string {
	var env []string
	for _, kv := range allowedEnv() {
		if !strings.HasPrefix(kv, protocol.PublishFileEnv+"=") {
			env = append(env, kv)
		}
	}
	if r.PublishFile != "" {
		env = append(env, protocol.PublishFileEnv+"="+filepath.Join(r.Checkout, r.PublishFile))
	}
	return env
}

// clearPublish removes a file the commit carries at the run's publish path,
// so only what the script writes is published. It goes through the
// checkout's os.Root: a link there is removed, never followed, and a
// directory is left alone. A path that cannot be removed is an error only
// when a regular file is still there to be read (readPublish refuses
// anything else: a directory, a path through a link out of the checkout).
func clearPublish(r record) error {
	if r.PublishFile == "" {
		return nil
	}
	root, err := os.OpenRoot(r.Checkout)
	if err != nil {
		return err
	}
	defer root.Close()
	err = root.Remove(r.PublishFile)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if st, serr := root.Lstat(r.PublishFile); serr == nil && st.Mode().IsRegular() {
		return err
	}
	return nil
}

// readPublish is what the run's script left in its publish file: nil when the
// run has none, the file is absent, is not a regular file, is over
// protocol.PublishMaxBytes or is not one JSON value (protocol.CleanPublish).
// It is opened through the checkout's os.Root, so no link, in the file's name
// or any directory above it, leads out of the checkout.
func readPublish(r record) json.RawMessage {
	if r.PublishFile == "" {
		return nil
	}
	root, err := os.OpenRoot(r.Checkout)
	if err != nil {
		return nil
	}
	defer root.Close()
	f, err := root.OpenFile(r.PublishFile, os.O_RDONLY|syscall.O_NONBLOCK, 0) // no block on a fifo
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, protocol.PublishMaxBytes+1))
	if err != nil {
		return nil
	}
	out, _ := protocol.CleanPublish(b)
	return out
}
