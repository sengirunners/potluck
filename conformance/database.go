package conformance

import (
	"fmt"
	"slices"

	"github.com/sengirunners/potluck/protocol"
)

const databaseRefusal = "database snapshots: malformed identity is refused before a box is lent"

// A portable conformance run has no owner's database artifact. It can still
// prove that a driver advertising the capability does not silently ignore the
// request. Valid snapshot startup is tested with each producer's artifact.
func (r *run) databaseSnapshots() {
	if r.kind != "box" {
		return
	}
	if !slices.Contains(boxCaps(r), protocol.CapDatabase) {
		r.skip(databaseRefusal, "no database-snapshot-v1 capability")
		return
	}
	_, e, err := r.acquire(args("request_id", r.key(), "actor", actor, "ttl_s", r.shortTTL(), "database", protocol.Database{Project: "potluck-conformance", Name: "invalid", Image: "mutable:latest", Manifest: "{}"}), r.cfg)
	if err == nil && (e == nil || e.Code != protocol.CodeBadRequest) {
		err = fmt.Errorf("malformed database answered %v, want bad_request", e)
	}
	r.add(databaseRefusal, err)
}
