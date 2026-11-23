package api

import (
	"encoding/json"
	"time"
)

// Round C of SPEC 17 (17.6): a team replica's copy and its outbox, as a
// member's daemon and its clients see them.

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
