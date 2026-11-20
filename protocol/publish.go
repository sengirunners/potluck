package protocol

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
)

// A run's publish file (docs/protocol.md, A run's publish): a script leaves
// JSON for its caller in one file, and the answer to status on the ended run
// carries it as `publish`.
const (
	// CapRunPublish: the driver reads the publish file named by ZM_PUBLISH_FILE
	// at acquire and answers it as the ended run's status `publish`.
	CapRunPublish = "run-publish"
	// PublishFileEnv is the environment variable the driver reads at acquire:
	// the file's path inside the run's working directory (the checkout). The
	// run's script gets it set to that file's absolute path.
	PublishFileEnv = "ZM_PUBLISH_FILE"
	// DefaultPublishFile is the publish file the daemon names when its own
	// environment names none, so every run a caller starts has one: at the top
	// of the checkout, where a script writes it without making a directory.
	DefaultPublishFile = ".potluck-publish.json"
	// PublishMaxBytes is the largest publish file that is read; a larger one
	// is omitted, never cut.
	PublishMaxBytes = 64 << 10
)

// PublishPathOK reports whether p is a path a publish file may be named by:
// clean, relative, inside the working directory, and not a shell string.
func PublishPathOK(p string) bool {
	return p != "" && len(p) <= 200 && p != "." && filepath.IsLocal(p) && filepath.Clean(p) == p &&
		!strings.ContainsAny(p, "\x00\r\n")
}

// CleanPublish is the one rule for what a run publishes: the bytes of b if
// they are within PublishMaxBytes and one valid JSON value other than null,
// compacted. Anything else is not published (ok false): a driver and the
// daemon both apply it, so a driver that does not is still bounded.
func CleanPublish(b []byte) (json.RawMessage, bool) {
	if len(b) == 0 || len(b) > PublishMaxBytes {
		return nil, false
	}
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil || bytes.Equal(buf.Bytes(), []byte("null")) {
		return nil, false
	}
	return json.RawMessage(buf.Bytes()), true
}
