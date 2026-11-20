package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// A run's publish (docs/protocol.md, A run's publish): with capability
// run-publish, a driver reads ZM_PUBLISH_FILE from its environment at
// acquire, a path inside the run's working directory, and answers the JSON the
// script left there as `publish` on the ended run's status. A driver without
// the capability skips the group.

const (
	pbEnded   = "an ended run's status carries the JSON its script wrote to the file ZM_PUBLISH_FILE named at acquire"
	pbKept    = "after release the ended run's status carries the same publish"
	pbHeld    = "a run still running publishes nothing, though its script wrote the file"
	pbBig     = "a publish file over 64 KiB is omitted: the run still ends with its exit"
	pbBad     = "a publish file that is not one JSON value is omitted: the run still ends with its exit"
	pbNoEnv   = "without ZM_PUBLISH_FILE at acquire the script is told of no file, and nothing is published"
	pbBadPath = "a ZM_PUBLISH_FILE that is not a clean path inside the working directory is refused: bad_request, and no run"
	// pbFile is the publish file the suite names: one level down, so the
	// driver makes (or the script makes) a directory, never just a file.
	pbFile = "out/publish.json"
)

// publishRun acquires a run of the probe with ZM_PUBLISH_FILE=file (unset when
// file is "") and answers its status once it has ended.
func (r *run) publishRun(file string, script ...string) (acquired, statusReport, error) {
	if file != "" {
		r.acquireEnv = []string{protocol.PublishFileEnv + "=" + file}
	}
	a, e, err := r.acquire(r.runArgs(r.key(), r.longTTL(), script...), r.cfg)
	r.acquireEnv = nil
	if err = must(e, err); err != nil {
		return a, statusReport{}, fmt.Errorf("acquire: %v", err)
	}
	st, err := r.pollState(a.Handle, time.Now().Add(r.opt.RunWait), "ended", "expired", "lost")
	return a, st, err
}

// samePublish says whether got is the JSON value want, however it is spaced.
func samePublish(got json.RawMessage, want string) error {
	var g, w any
	if len(got) == 0 {
		return fmt.Errorf("status has no publish, want %s", want)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		return fmt.Errorf("publish %q is not JSON: %v", got, err)
	}
	_ = json.Unmarshal([]byte(want), &w)
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if !bytes.Equal(gb, wb) {
		return fmt.Errorf("publish %s, want %s", got, want)
	}
	if len(got) > protocol.PublishMaxBytes {
		return fmt.Errorf("publish is %d bytes, over %d", len(got), protocol.PublishMaxBytes)
	}
	return nil
}

func (r *run) publish() {
	if r.kind != "run" {
		r.skip("a run's publish", "run targets only")
		return
	}
	caps := []string{}
	if r.desc.Capabilities != nil {
		caps = *r.desc.Capabilities
	}
	if !slices.Contains(caps, protocol.CapRunPublish) {
		r.skip("a run's publish", "describe lists no run-publish capability")
		return
	}
	const want = `{"ok":true,"urls":["https://example.test/a"],"n":2}`
	a, st, err := r.publishRun(pbFile, "publish", "write", want)
	if err == nil {
		err = wantState(st, "ended", ptr(0))
	}
	if err == nil {
		err = samePublish(st.Publish, want)
	}
	if r.add(pbEnded, err) {
		rel, err := r.release(a.Handle)
		if err == nil {
			err = wantRelease(rel, false, ptr(0))
		}
		if err == nil {
			var again statusReport
			if again, err = r.status(a.Handle); err == nil {
				err = samePublish(again.Publish, want)
			}
		}
		if r.add(pbKept, err) {
			delete(r.live, a.Handle)
		}
	}
	r.add(pbHeld, r.publishHeld())
	for _, c := range []struct {
		name   string
		script []string
	}{
		{pbBig, []string{"publish", "big"}},
		{pbBad, []string{"publish", "write", `{"a":`}},
		{pbBad, []string{"publish", "write", `{"a":1} {"b":2}`}},
	} {
		_, st, err := r.publishRun(pbFile, c.script...)
		if err == nil {
			err = wantState(st, "ended", ptr(0))
		}
		if err == nil && len(st.Publish) > 0 {
			err = fmt.Errorf("status carries publish (%d bytes), want none", len(st.Publish))
		}
		r.add(c.name, err)
	}
	_, st, err = r.publishRun("", "publish", "write", `{"a":1}`)
	if err == nil {
		err = wantState(st, "ended", ptr(9)) // the script: ZM_PUBLISH_FILE is not set
	}
	if err == nil && len(st.Publish) > 0 {
		err = fmt.Errorf("status carries publish %s, want none", st.Publish)
	}
	r.add(pbNoEnv, err)
	r.add(pbBadPath, r.publishBadPath())
}

// publishHeld checks a run that wrote its file and goes on running.
func (r *run) publishHeld() error {
	r.acquireEnv = []string{protocol.PublishFileEnv + "=" + pbFile}
	a, e, err := r.acquire(r.runArgs(r.key(), r.longTTL(), "publish", "hold", `{"early":true}`), r.cfg)
	r.acquireEnv = nil
	if err = must(e, err); err != nil {
		return fmt.Errorf("acquire: %v", err)
	}
	defer r.end(a.Handle)
	for end := time.Now().Add(3 * tick); ; r.sleep(tick) {
		st, err := r.status(a.Handle)
		if err != nil {
			return err
		}
		if st.State != "running" {
			return fmt.Errorf("state %q, want running: the script holds", st.State)
		}
		if len(st.Publish) > 0 {
			return fmt.Errorf("a running run's status carries publish %s", st.Publish)
		}
		if !time.Now().Before(end) {
			return nil
		}
	}
}

// publishBadPath sends paths that leave the working directory or are not
// plain paths.
func (r *run) publishBadPath() error {
	for _, p := range []string{"../publish.json", "/tmp/publish.json", "a/../publish.json"} {
		r.acquireEnv = []string{protocol.PublishFileEnv + "=" + p}
		key := r.key()
		a, e, err := r.acquire(r.runArgs(key, r.longTTL(), "exit", "0"), r.cfg)
		r.acquireEnv = nil
		if err == nil && e == nil {
			r.endHeld(held{acquired: a})
			return fmt.Errorf("ZM_PUBLISH_FILE=%s: acquire answered a handle", p)
		}
		if err = wantCode(e, err, protocol.CodeBadRequest); err != nil {
			return fmt.Errorf("ZM_PUBLISH_FILE=%s: %v", p, err)
		}
		es, err := r.list()
		if err != nil {
			return err
		}
		for _, x := range es {
			if x.RequestID == key {
				return fmt.Errorf("ZM_PUBLISH_FILE=%s: refused, and list shows %s", p, x.Handle)
			}
		}
	}
	return nil
}
