package api

import (
	"encoding/json"
	"time"
)

// Round C of SPEC 17 (17.6): a team replica's copy, its sync and its outbox.

// Snapshot line kinds (GET /v0/sync/snapshot, NDJSON).
const (
	SnapEnd   = "end"   // the last line: Cursor and Now
	SnapError = "error" // the stream failed (Data: ErrorResponse); no end follows
)

// SnapshotLine is one line of GET /v0/sync/snapshot. A stream without its
// end line is incomplete and must be discarded.
type SnapshotLine struct {
	Kind   string          `json:"kind"`
	Data   json.RawMessage `json:"data,omitempty"`
	Cursor string          `json:"cursor,omitempty"`
	Now    *time.Time      `json:"now,omitempty"`
	// Generation (end line) is the home's restore generation, "" when it
	// was never restored. A replica keeps it with the copy and sends it
	// back as GET /v0/sync's gen= (SPEC 17.6 "Resync").
	Generation string `json:"generation,omitempty"`
}

// QueuedWrite is one write from a replica's outbox. The authority runs it as
// Method Path with Body, Zm-As As and Zm-Write-Id ID, in the order given,
// and records its answer under (identity, ID), so a replay answers the
// recorded result instead of running it again.
type QueuedWrite struct {
	ID     string          `json:"id"` // a ULID the replica minted
	Method string          `json:"method"`
	Path   string          `json:"path"` // /v0/work..., /v0/leases..., /v0/mail...
	Body   json.RawMessage `json:"body,omitempty"`
	As     string          `json:"as,omitempty"`
	// Item is the work item or lease key the write touches (its base and
	// what it left behind are read for it).
	Item string `json:"item,omitempty"`
	// Base, on a field write (status), is a change id for Item the replica
	// had seen: the write is refused (conflict) when the field changed at
	// the authority after it.
	Base string `json:"base,omitempty"`
}

// WritesRequest is POST /v0/team/writes.
type WritesRequest struct {
	Project string        `json:"project"`
	Writes  []QueuedWrite `json:"writes"`
}

// WriteResult is one write's answer: the HTTP status and body the route
// gave (an ErrorResponse when not 2xx). Seen is the newest change id about
// Item once it ran (the replica's copy has caught up with the write when its
// row reaches it). Replayed: the recorded answer of an earlier run.
type WriteResult struct {
	ID       string          `json:"id"`
	Status   int             `json:"status"`
	Body     json.RawMessage `json:"body"`
	Seen     string          `json:"seen,omitempty"`
	Replayed bool            `json:"replayed,omitempty"`
	// StatusSeen is the id of the status_changed event the write minted (a
	// status write, or a claim that moved the item to claimed): a later
	// queued status write of the item may take it as its base. Other writes'
	// events never raise a base (they would hide another writer's status
	// change the replica never saw).
	StatusSeen string `json:"status_seen,omitempty"`
}

// WritesResponse answers the writes that ran, in order. Stopped says why the
// rest did not (the authority could not run one now: retry from it, in order).
type WritesResponse struct {
	Results []WriteResult `json:"results"`
	Stopped string        `json:"stopped,omitempty"`
}

// Outbox entry states on a replica.
const (
	OutboxPending  = "pending"  // waiting for the authority
	OutboxAcked    = "acked"    // the authority applied it; the copy has not caught up yet
	OutboxConflict = "conflict" // the authority refused it (a claim lost, a stale base): shown, never retried
	OutboxFailed   = "failed"   // refused for another reason (not found, forbidden, bad request): shown, never retried
)

// OutboxEntry is one queued write as GET /v0/team/outbox shows it.
type OutboxEntry struct {
	ID      string          `json:"id"`
	Project ProjectRef      `json:"project"`
	Kind    string          `json:"kind"` // claim, release, comment, status, mail, reply, ack, link, lease, create
	Item    string          `json:"item,omitempty"`
	As      string          `json:"as,omitempty"`
	State   string          `json:"state"`
	Error   string          `json:"error,omitempty"` // conflict or failed: what the authority said
	Created time.Time       `json:"created"`
	Done    *time.Time      `json:"done,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
}

// OutboxList is GET /v0/team/outbox.
type OutboxList struct {
	Entries []OutboxEntry `json:"entries"`
}

// ReplicaStatus is a joined project's copy, as Remote.Replica shows it.
type ReplicaStatus struct {
	State     string     `json:"state"` // loading | ok | stale
	Cursor    string     `json:"cursor,omitempty"`
	Synced    *time.Time `json:"synced,omitempty"`     // the last answer from the authority
	AgeSecs   float64    `json:"age_seconds"`          // since Synced (an age, never a count)
	Reachable bool       `json:"reachable"`            // the last call to the authority got an answer
	Pending   int        `json:"pending"`              // outbox entries waiting
	Conflicts int        `json:"conflicts,omitempty"`  // refused entries kept for the record
	LastError string     `json:"last_error,omitempty"` // sanitised
}

// QueuedResponse answers a write the replica queued while its authority was
// unreachable (a claim, a release, a lease): it is not done yet. Its outcome
// shows in GET /v0/team/outbox; a queued claim that loses is a conflict there.
type QueuedResponse struct {
	Queued  bool   `json:"queued"`
	Write   string `json:"write"` // the outbox entry's id
	Project string `json:"project"`
	Item    string `json:"item,omitempty"`
	Note    string `json:"note"`
}

// WriteCancelResponse is POST /v0/team/writes/{id}/cancel: the write id is
// now settled at the authority. "cancelled": it never ran and never will (a
// delivery of that id answers 409). "ran": it already ran; Status is its
// recorded status (never its body).
type WriteCancelResponse struct {
	State  string `json:"state"` // WriteCancelled | WriteRan
	Status int    `json:"status,omitempty"`
}

const (
	WriteCancelled = "cancelled"
	WriteRan       = "ran"
)

// NoQueueData is the data of a 503 for a no-queue write (Zm-No-Queue) that
// got no answer. Outcome: "not_sent" (the authority was unreachable, nothing
// went out), "cancelled" (it went out, got no answer, and the authority
// confirmed it never ran) or "unknown" (no answer and no confirmation: it may
// have applied; zm project team cancel <write> settles it).
type NoQueueData struct {
	Write   string `json:"write,omitempty"`
	Outcome string `json:"outcome"`
}

const (
	OutcomeNotSent   = "not_sent"
	OutcomeCancelled = "cancelled"
	OutcomeUnknown   = "unknown"
)
