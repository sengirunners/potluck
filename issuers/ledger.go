package issuers

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/sengirunners/potluck/kit/store"
)

// Ledger events.
const (
	EventIssued           = "issued"
	EventRenewed          = "renewed"
	EventRevoked          = "revoked"
	EventRefused          = "refused"
	EventFailed           = "failed"            // a mint, delivery, renewal or revocation that did not happen; why says
	EventExpired          = "expired"           // lapsed at the issuer without a revoke (renewal failed until expiry)
	EventExpiredUnrevoked = "expired_unrevoked" // held in memory by a run that ended without revoking it
	// EventMaybeDelivered is a delivery that failed after it was sent: the
	// value may be in the box (rt.ErrMaybeDelivered). The token is revoked
	// after it (a revoked row follows), and Issue answers an error wrapping
	// rt.ErrMaybeDelivered so no caller delivers it again as if nothing came.
	EventMaybeDelivered = "maybe_delivered"
)

// Entry is one ledger row. It never holds a value: a minted credential is
// named by its hashed_token, the base64 of its SHA-256, the form GitHub's
// audit log searches by (ghapp.Token.HashedToken).
type Entry struct {
	At          time.Time
	Event       string
	Issuer      string
	Kind        string
	Subject     string // identity:<name> or job:<repo>:<workflow>@<ref>#<run>
	Name        string
	Project     string // the pool project's ULID
	Area        string
	Box         string
	Host        string
	HostClass   string
	Repo        string
	Permissions string // "contents=write,metadata=read"
	ExpiresAt   time.Time
	HashedToken string
	Elevated    string // the elevated permissions granted at write: "workflows,checks"
	Reason      string // the request's reason
	RuleReason  string // the ceiling rule's reason (a rule with elevated permissions has one)
	Why         string // a refusal's, failure's or end's cause
	Grant       string // the grant id the request named
	Job         *Job   `json:"-"` // published, not stored
}

// Ledger is the issuer's recovery record (the audit surface is the pool's
// event log, Options.Events). It records entries durably and answers what a previous run left
// outstanding: minted, never revoked, expired or settled. It also keeps the
// assignments: which box holds which request (never a token), written when a
// token is delivered, removed when the box ends, kept across a stop, so
// Start can issue again.
type Ledger interface {
	Record(ctx context.Context, e Entry) error
	Outstanding(ctx context.Context, issuer string) ([]Entry, error)
	Assign(ctx context.Context, issuer string, req Request) error
	// Unassign removes box's assignment under name, or all of box's when name is "".
	Unassign(ctx context.Context, issuer string, box Box, name string) error
	Assigned(ctx context.Context, issuer string) ([]Request, error)
}

// Migrations are the ledger's tables in registry.db (NewModule adds them).
var Migrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "issuers/0001_ledger", Script: `CREATE TABLE issuers_ledger (
	id           INTEGER PRIMARY KEY,
	at           INTEGER NOT NULL,
	event        TEXT NOT NULL,
	issuer       TEXT NOT NULL,
	kind         TEXT NOT NULL,
	subject      TEXT NOT NULL,
	name         TEXT NOT NULL,
	project      TEXT NOT NULL,
	area         TEXT NOT NULL,
	box          TEXT NOT NULL,
	host         TEXT NOT NULL,
	host_class   TEXT NOT NULL,
	repo         TEXT NOT NULL,
	permissions  TEXT NOT NULL,
	expires      INTEGER NOT NULL,
	hashed_token TEXT NOT NULL,
	elevated     TEXT NOT NULL,
	reason       TEXT NOT NULL,
	rule_reason  TEXT NOT NULL,
	why          TEXT NOT NULL,
	grant_id     TEXT NOT NULL
);
CREATE INDEX issuers_ledger_token ON issuers_ledger (hashed_token);
CREATE INDEX issuers_ledger_box ON issuers_ledger (project, box, id);
CREATE TABLE issuers_assignments (
	issuer  TEXT NOT NULL,
	project TEXT NOT NULL,
	box     TEXT NOT NULL,
	name    TEXT NOT NULL,
	request TEXT NOT NULL,
	created INTEGER NOT NULL,
	PRIMARY KEY (issuer, project, box, name)
);`},
}

// SQLLedger is the ledger in the registry (kit/store: issuers_ledger).
type SQLLedger struct{ DB *sql.DB }

const maxField = 512

func clip(s string) string {
	if len(s) > maxField {
		return s[:maxField]
	}
	return s
}

func (l SQLLedger) Record(ctx context.Context, e Entry) error {
	var exp int64
	if !e.ExpiresAt.IsZero() {
		exp = e.ExpiresAt.Unix()
	}
	_, err := l.DB.ExecContext(ctx, `INSERT INTO issuers_ledger (at, event, issuer, kind, subject, name, project, area, box, host, host_class,
		repo, permissions, expires, hashed_token, elevated, reason, rule_reason, why, grant_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.At.UnixMilli(), e.Event, clip(e.Issuer), e.Kind, clip(e.Subject), clip(e.Name), clip(e.Project), clip(e.Area), clip(e.Box), clip(e.Host),
		clip(e.HostClass), clip(e.Repo), clip(e.Permissions), exp, e.HashedToken, clip(e.Elevated), clip(e.Reason), clip(e.RuleReason), clip(e.Why),
		clip(e.Grant))
	return err
}

func (l SQLLedger) Outstanding(ctx context.Context, issuer string) ([]Entry, error) {
	rows, err := l.DB.QueryContext(ctx, `SELECT at, event, issuer, kind, subject, name, project, area, box, host, host_class, repo, permissions,
		expires, hashed_token, elevated, reason, rule_reason, why, grant_id FROM issuers_ledger m
		WHERE m.issuer = ? AND m.event IN ('issued', 'renewed') AND m.hashed_token != ''
		AND NOT EXISTS (SELECT 1 FROM issuers_ledger e WHERE e.hashed_token = m.hashed_token
			AND e.event IN ('revoked', 'expired', 'expired_unrevoked'))
		ORDER BY m.id`, issuer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var at, exp int64
		if err := rows.Scan(&at, &e.Event, &e.Issuer, &e.Kind, &e.Subject, &e.Name, &e.Project, &e.Area, &e.Box, &e.Host, &e.HostClass, &e.Repo,
			&e.Permissions, &exp, &e.HashedToken, &e.Elevated, &e.Reason, &e.RuleReason, &e.Why, &e.Grant); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at).UTC()
		if exp != 0 {
			e.ExpiresAt = time.Unix(exp, 0).UTC()
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (l SQLLedger) Assign(ctx context.Context, issuer string, req Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = l.DB.ExecContext(ctx, `INSERT INTO issuers_assignments (issuer, project, box, name, request, created) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (issuer, project, box, name) DO UPDATE SET request = excluded.request`,
		issuer, req.Box.Project, req.Box.ID, req.Name, string(b), time.Now().Unix())
	return err
}

func (l SQLLedger) Unassign(ctx context.Context, issuer string, box Box, name string) error {
	_, err := l.DB.ExecContext(ctx, `DELETE FROM issuers_assignments WHERE issuer = ? AND project = ? AND box = ? AND (? = '' OR name = ?)`,
		issuer, box.Project, box.ID, name, name)
	return err
}

func (l SQLLedger) Assigned(ctx context.Context, issuer string) ([]Request, error) {
	rows, err := l.DB.QueryContext(ctx, `SELECT request FROM issuers_assignments WHERE issuer = ? ORDER BY created, project, box, name`, issuer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		var r Request
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			continue // a row this version cannot read is never issued
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
