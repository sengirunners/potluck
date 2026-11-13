package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// Authority-only types: a module registers the type prefixes of events
// that must stay on the machine that holds the log (never copied to a
// replica, never exported) and the top-level keys a reader over a tunnel
// never receives. The rule is enforced by type at read time, whatever an
// event's stored audience says (rows written before a module registered
// keep working the safe way), and New rewrites stored audiences once to
// match. The base names no module's types: modules register at init.
//
// The registration is the module's init: a binary that builds the event bus
// without importing the module that registers a prefix (package grants for
// the secrets ledger) has no rule for it, and such events, if any are in
// its logs, are treated by their stored audience alone. Another daemon
// compiles these packages: it gets the rule by linking grants (which run
// targets import).

// TypeRule is one registered prefix's rule.
type TypeRule struct {
	Prefix string
	// Withheld are the keys a reader over a tunnel does not receive.
	Withheld []string
	// CountOf, when set, names an array key: a reader over a tunnel gets
	// "count" (its length) when the event carries none.
	CountOf string
}

var (
	typeMu    sync.RWMutex
	typeRules []TypeRule
)

// RegisterAuthorityOnly registers r (at init; one rule per prefix).
func RegisterAuthorityOnly(r TypeRule) {
	typeMu.Lock()
	defer typeMu.Unlock()
	if r.Prefix == "" || slices.ContainsFunc(typeRules, func(x TypeRule) bool { return x.Prefix == r.Prefix }) {
		panic("events: authority-only prefix empty or registered twice: " + r.Prefix)
	}
	typeRules = append(typeRules, r)
}

// typeRule is typ's rule, if a registered prefix covers it.
func typeRule(typ string) (TypeRule, bool) {
	typeMu.RLock()
	defer typeMu.RUnlock()
	for _, r := range typeRules {
		if strings.HasPrefix(typ, r.Prefix) {
			return r, true
		}
	}
	return TypeRule{}, false
}

// stayHome is whether x stays on the machine that holds the log: its
// stored audience says so, or its type is registered.
func stayHome(x stored) bool {
	_, ok := typeRule(x.ev.Type)
	return ok || x.audience.authorityOnly()
}

// stayHomeType is stayHome from a row's type and stored audience JSON.
func stayHomeType(typ, audience string) bool {
	if _, ok := typeRule(typ); ok {
		return true
	}
	if audience == "" {
		return false
	}
	var a Audience
	return json.Unmarshal([]byte(audience), &a) != nil || a.AuthorityOnly // unreadable: the safe way
}

// overTunnel withholds what a reader over a tunnel never receives: the
// stored audience's keys and the type rule's, adding a count first.
func overTunnel(x stored, e api.Event) api.Event {
	var keys []string
	if x.audience != nil {
		keys = append(keys, x.audience.NotOverTunnel...)
	}
	r, ok := typeRule(e.Type)
	if ok {
		keys = append(keys, r.Withheld...)
	}
	if len(keys) == 0 {
		return e
	}
	if ok && r.CountOf != "" {
		var m map[string]json.RawMessage
		if json.Unmarshal(e.Data, &m) == nil {
			if _, has := m["count"]; !has {
				var arr []json.RawMessage
				_ = json.Unmarshal(m[r.CountOf], &arr)
				m["count"], _ = json.Marshal(len(arr))
				e.Data, _ = json.Marshal(m)
			}
		}
	}
	return withoutKeys(e, keys)
}

// authoritySQL is a WHERE fragment (and args) true for rows that stay on
// the machine that holds the log.
func authoritySQL() (string, []any) {
	typeMu.RLock()
	defer typeMu.RUnlock()
	parts := []string{`coalesce(json_extract(NULLIF(audience,''),'$.authority_only'), 0) = 1`}
	var args []any
	for _, r := range typeRules {
		parts = append(parts, `substr(type, 1, ?) = ?`)
		args = append(args, len(r.Prefix), r.Prefix)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// typeRange is a WHERE fragment matching r's types by range, so the
// events_type index serves it.
func typeRange(r TypeRule) (string, []any) {
	return `type >= ? AND type < ?`, []any{r.Prefix, r.Prefix + "\U0010FFFF"}
}

// pruneLedgerTx is the authority-only rows' own retention (LedgerKeep,
// LedgerKeepFor): per registered prefix, the oldest past keep and any
// older than cutoff go. It never moves pruned_upto: no replica holds them.
func pruneLedgerTx(ctx context.Context, tx *sql.Tx, keep int, cutoff time.Time) error {
	typeMu.RLock()
	rules := slices.Clone(typeRules)
	typeMu.RUnlock()
	for _, r := range rules {
		w, args := typeRange(r)
		if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE `+w+` AND time < ?`, append(args, store.Millis(cutoff))...); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE `+w, args...).Scan(&n); err != nil {
			return err
		}
		if n > keep {
			if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (SELECT id FROM events WHERE `+w+` ORDER BY id LIMIT ?)`,
				append(args, n-keep)...); err != nil {
				return err
			}
		}
	}
	return nil
}

// sealStored rewrites, in one transaction, the stored audience of every row
// a registered prefix covers that does not yet say authority-only (rows
// written before the rule existed), and answers how many. It returns at
// once when none does. Idempotent.
func sealStored(ctx context.Context, db *sql.DB) (int, error) {
	typeMu.RLock()
	rules := slices.Clone(typeRules)
	typeMu.RUnlock()
	const unflagged = ` AND coalesce(json_extract(NULLIF(audience,''),'$.authority_only'), 0) != 1`
	any := false
	for _, r := range rules {
		w, args := typeRange(r)
		var one int
		err := db.QueryRowContext(ctx, `SELECT 1 FROM events WHERE `+w+unflagged+` LIMIT 1`, args...).Scan(&one)
		if err == nil {
			any = true
			break
		}
		if err != sql.ErrNoRows {
			return 0, err
		}
	}
	if !any {
		return 0, nil
	}
	n := 0
	err := store.Tx(ctx, db, func(tx *sql.Tx) error {
		for _, r := range rules {
			w, args := typeRange(r)
			rows, err := tx.QueryContext(ctx, `SELECT id, audience FROM events WHERE `+w+unflagged, args...)
			if err != nil {
				return err
			}
			type fix struct{ id, audience string }
			var fixes []fix
			for rows.Next() {
				var id, au string
				if err := rows.Scan(&id, &au); err != nil {
					rows.Close()
					return err
				}
				a := Audience{}
				if au != "" && json.Unmarshal([]byte(au), &a) != nil {
					a = Audience{Need: authz.Admin} // unreadable: the strictest
				}
				a.AuthorityOnly = true
				for _, k := range r.Withheld {
					if !slices.Contains(a.NotOverTunnel, k) {
						a.NotOverTunnel = append(a.NotOverTunnel, k)
					}
				}
				b, _ := json.Marshal(a)
				fixes = append(fixes, fix{id, string(b)})
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, f := range fixes {
				if _, err := tx.ExecContext(ctx, `UPDATE events SET audience = ? WHERE id = ?`, f.audience, f.id); err != nil {
					return err
				}
			}
			n += len(fixes)
		}
		return nil
	})
	return n, err
}
