package refdriver

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/sengirunners/potluck/protocol"
)

// A run's publish (docs/protocol.md, A run's publish). The driver reads
// ZM_PUBLISH_FILE at acquire, a path inside the run's checkout; the script
// gets the file's absolute path in its own ZM_PUBLISH_FILE; the ended run's
// status answers what the script wrote there. Bugs, one for each check of the
// suite's publish group:
//
//	publish-ignored        never reads ZM_PUBLISH_FILE: the script is told of no file
//	publish-no-env         reads it, but the script is not told of the file
//	publish-always-env     tells the script of a file even when none was named, and publishes it
//	publish-early          a running run's status carries the file
//	publish-uncapped       no size cap
//	publish-unchecked      the file's bytes whether or not they are one JSON value
//	publish-lost           the answer after release has no publish
//	publish-escapes        a path outside the checkout is taken
//
// no-publish is not a bug: a driver without the capability must pass.

func (d *drv) publishServed() bool { return !d.is("no-publish") }

// publishDescribe lists the capability of a run driver that serves it.
func (d *drv) publishDescribe(out map[string]any) {
	if out["kind"] == "run" && d.publishServed() {
		out["capabilities"] = append(out["capabilities"].([]string), protocol.CapRunPublish)
	}
}

// publishAcquire is the path ZM_PUBLISH_FILE names in this call's environment.
func (d *drv) publishAcquire() (string, error) {
	if !d.publishServed() || d.is("publish-ignored") {
		return "", nil
	}
	p := os.Getenv(protocol.PublishFileEnv)
	switch {
	case p == "" && d.is("publish-always-env"):
		return "default-publish.json", nil
	case p != "" && !protocol.PublishPathOK(p) && !d.is("publish-escapes"):
		return "", refuse("bad_request", "%s must be a clean path inside the run's working directory", protocol.PublishFileEnv)
	}
	return p, nil
}

// publishEnv is the script's environment: the driver's own, with
// ZM_PUBLISH_FILE the file's absolute path (or unset).
func (d *drv) publishEnv(r record) []string {
	var env []string
	for _, kv := range os.Environ() {
		if len(kv) <= len(protocol.PublishFileEnv) || kv[:len(protocol.PublishFileEnv)+1] != protocol.PublishFileEnv+"=" {
			env = append(env, kv)
		}
	}
	if r.PublishFile != "" && !d.is("publish-no-env") {
		env = append(env, protocol.PublishFileEnv+"="+filepath.Join(r.Checkout, r.PublishFile))
	}
	return env
}

// readPublish is what the script left in its file, by the protocol's rule
// (the bugs break it).
func readPublish(r record, bug string) json.RawMessage {
	if r.PublishFile == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(r.Checkout, r.PublishFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	max := int64(protocol.PublishMaxBytes + 1)
	if bug == "publish-uncapped" {
		max = 1 << 30
	}
	b, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return nil
	}
	if bug == "publish-unchecked" {
		if len(b) == 0 || len(b) > protocol.PublishMaxBytes {
			return nil
		}
		return json.RawMessage(b) // answered as it is: the driver's own answer is then not JSON
	}
	if bug == "publish-uncapped" {
		if json.Valid(b) {
			return json.RawMessage(b)
		}
		return nil
	}
	out, _ := protocol.CleanPublish(b)
	return out
}

// publishStatus adds what status says of the file: for an ended run the
// publish read when it ended; the bug publish-early reads it while it runs.
func (d *drv) publishStatus(r record, out map[string]any) {
	switch {
	case r.State == "ended" && len(r.Publish) > 0 && !(d.is("publish-lost") && r.Releases > 0):
		out["publish"] = r.Publish
	case d.is("publish-early") && r.State == "running":
		if p := readPublish(r, ""); len(p) > 0 {
			out["publish"] = p
		}
	}
}
