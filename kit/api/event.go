package api

import (
	"encoding/json"
	"time"
)

// Event is a CloudEvents 1.0 JSON envelope. Every event in zm uses it, on
// the SSE stream (GET /v0/events) and in storage.
//
// ID is a ULID, unique across every log on the daemon and increasing in
// publish order (with random increments, so gaps reveal nothing). It is also
// the SSE id and the resume cursor (Last-Event-ID). There is no sequence
// number on the wire: a filtered subscriber must not be able to count events
// it cannot see.
type Event struct {
	SpecVersion     string          `json:"specversion"` // always "1.0"
	ID              string          `json:"id"`          // ULID; the resume cursor
	Source          string          `json:"source"`      // "zm://<machine>"
	Type            string          `json:"type"`        // dev.zm.<module>.<what>, see the Event* constants
	Subject         string          `json:"subject,omitempty"`
	Time            time.Time       `json:"time"`
	DataContentType string          `json:"datacontenttype,omitempty"` // "application/json"
	Data            json.RawMessage `json:"data,omitempty"`
	// Project is the CloudEvents extension "zmproject": the project ULID,
	// or "none" for machine-level events (visible only to the machine's owner).
	Project string `json:"zmproject"`
	// Area is the extension "zmarea": the area inside the project, "" = none.
	Area string `json:"zmarea,omitempty"`
}

// EventTypePrefix starts every zm event type.
const EventTypePrefix = "dev.zm."

// Core event types (daemon, projects, authz). Module event types live in each module's file.
const (
	EventDaemonStarted  = "dev.zm.daemon.started"  // machine-level; data Health
	EventDaemonStopping = "dev.zm.daemon.stopping" // machine-level; data {by, what, force}
	EventProjectCreated = "dev.zm.project.created" // data Project
	EventProjectUpdated = "dev.zm.project.updated" // data Project
	EventAreaAdded      = "dev.zm.project.area_added"
	EventGranted        = "dev.zm.authz.granted"       // machine-level; data Grant
	EventRevoked        = "dev.zm.authz.revoked"       // machine-level; data Grant
	EventTokenMinted    = "dev.zm.authz.token_minted"  // machine-level; data Token (never the secret)
	EventTokenRevoked   = "dev.zm.authz.token_revoked" // machine-level; data Token
)

// EventAreaUpdated is an area's rules changing (zm area set); data Area.
const EventAreaUpdated = "dev.zm.project.area_updated"

// EventQuery is GET /v0/events's query string.
type EventQuery struct {
	// Types is a comma list of type prefixes, e.g. "dev.zm.mail.,dev.zm.session.state_changed".
	Types string
	// Subject keeps only events with this subject (exact).
	Subject string
	// Project keeps only this project (slug or ULID). Empty = every project the caller can see.
	Project string
	// Since replays stored events after this event id (also: Last-Event-ID
	// header) before going live. "" = no replay; "0" = everything stored.
	Since string
	// NoLive returns after the replay (GET /v0/events?live=0) instead of streaming.
	NoLive bool
}
