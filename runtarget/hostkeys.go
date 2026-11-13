package runtarget

// Which team project a host's shares and handles belong to: the authority's
// project id, never the slug this machine joined it under. A slug is only a
// local name: leave a project and join an unrelated one under the same slug,
// and anything keyed by the slug would pass to the new project.
//
// Builds before this keyed run_targets.toml's share lists and
// host_handles.json's entries by slug; their files are told apart by format
// (no format line, no version), never by the shape of a value, since a slug
// can look like an id. migrateHostKeys moves them on, once, when the module
// starts (retried on each host call until it finishes):
// each slug becomes the id of the project this machine has recorded under
// it (joined, or revoked for now; teamlink loads every join before this
// module starts), and that project keeps the request key tag it had, so its
// boxes and volumes on the host still answer to it. The mapping is frozen
// in host_keys.json (hostKeysFile), written first, then marked done in
// registry.db (hostMarked), where no restore of the files reaches: from then on a slug
// resolves only through it, never through the projects joined now, so a
// project joined later under that slug gets nothing, even from a restored
// backup of the old files. A slug no project was recorded under is dropped
// from the shares; a ledger entry left with a slug matches no project.
//
// One limit: a host that left a project and joined another under its slug
// before the migration finished gives the second what the first left (the
// old files do not say which project a slug meant).

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/atomicfile"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// joinedProjects are the team projects this machine records a join to,
// slug -> project id: all of them, or (live) only those whose access has not
// ended. A slug two recorded projects share names neither.
func (s *Service) joinedProjects(live bool) map[string]string {
	out := map[string]string{}
	if s.link == nil {
		return out
	}
	twice := map[string]bool{}
	for _, x := range s.link.List() {
		snap := x.Snapshot()
		if snap.Project.ID == "" || (live && teamlink.Ended(snap) != nil) {
			continue
		}
		if id, ok := out[snap.Project.Slug]; ok && id != snap.Project.ID {
			twice[snap.Project.Slug] = true
		}
		out[snap.Project.Slug] = snap.Project.ID
	}
	for slug := range twice {
		delete(out, slug)
	}
	return out
}

// resolveShare is a bind's share list as project ids: an id is kept, a slug
// must name a project this machine is in.
func (s *Service) resolveShare(share []string) ([]string, error) {
	joined := s.joinedProjects(true)
	var out []string
	for _, p := range share {
		// The slug of a project this machine is in first: a slug can look
		// like an id (26 digits).
		id := joined[p]
		if id == "" && ids.Valid(p) {
			id = p
		}
		if id == "" {
			return nil, api.Errorf(api.CodeBadRequest, "share %q: this machine is not in a team project of that name", p).
				WithHint("join the project first (potluck join <code>), then share with it")
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// sharedNames is a share list as the owner reads it: the slug of each
// project this machine records a join to, else the project's id.
func (s *Service) sharedNames(share []string) []string {
	slugOf := map[string]string{}
	for slug, id := range s.joinedProjects(false) {
		slugOf[id] = slug
	}
	out := make([]string, len(share))
	for i, id := range share {
		out[i] = cmp.Or(slugOf[id], id)
	}
	return out
}

// hostKey is the request key the host sends its driver for request_id of
// the project whose key is key (projectKey): a tag of the project first, so
// two projects sharing a binding never meet on one key (the authority's key
// already carries the actor).
func hostKey(key, requestID string) string {
	sum := sha256.Sum256([]byte("project\x00" + key))
	return hex.EncodeToString(sum[:5]) + "-" + requestID
}

// projectKey is what hostKey tags project's keys with: the slug it was
// migrated from (its boxes and volumes made before keys carried the id),
// else its id behind a NUL, which no slug holds: a slug can look like an id
// (26 digits), so the two never share a tag. Once a forget has dropped what
// this host kept for the id (dropForgotten), the tag also carries the id's
// key epoch, so whoever the id is shared with next meets none of the old
// volumes or request keys. Call after hostReady; an error (the registry
// cannot be read) refuses the call.
func (s *Service) projectKey(ctx context.Context, project string) (string, error) {
	key := "id\x00" + project
	if slug := s.host.legacy[project]; slug != "" {
		key = slug
	}
	var epoch string
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT epoch FROM run_key_epochs WHERE project = ?`, project).Scan(&epoch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return key, nil
	case err != nil:
		return "", api.Errorf(api.CodeUnavailable, "this host's key epochs cannot be read: %v", err)
	}
	// After a NUL, which neither a slug nor an id holds.
	return key + "\x00epoch " + epoch, nil
}

// raiseKeyEpoch gives project a key tag it never had (projectKey): a new
// random epoch each time, never a count, so a drop after registry.db was
// restored (taking an epoch back) still makes a tag no one has used.
func (s *Service) raiseKeyEpoch(ctx context.Context, project string) error {
	_, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO run_key_epochs (project, epoch) VALUES (?, ?)
ON CONFLICT (project) DO UPDATE SET epoch = excluded.epoch`, project, ids.New())
	return err
}

// hostKeysFile is what the migration froze, written once (before the ledger
// and the bindings are rewritten) and never again: which project each old
// slug meant, as legacy_keys (project id -> slug). Its being there means the
// migration happened, so an old-format ledger or bindings file found later
// (a restored backup) resolves only through it, never through the projects
// joined now.
const hostKeysFile = "host_keys.json"

// The migration's mark in registry.db (hostMarked): set once host_keys.json
// is written. registry.db holds the joins too (teamlink's team_remotes), so
// a restore that takes the mark back (the whole home, or registry.db alone)
// takes the joins back with it, and a migration run again then means the
// projects the old files meant; a restore of the files alone leaves the mark,
// and the host refuses to migrate them again against today's joins.
var hostKeyMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "runtarget/0001_host_keys", Script: `CREATE TABLE run_host_keys (key TEXT PRIMARY KEY, value TEXT NOT NULL);`},
	// A project id's key epoch (projectKey): none until a forget drops
	// what this host kept for the id.
	{Kind: store.KindRegistry, Name: "runtarget/0002_key_epochs", Script: `CREATE TABLE run_key_epochs (project TEXT PRIMARY KEY, epoch TEXT NOT NULL);`},
}

type hostKeys struct {
	LegacyKeys map[string]string `json:"legacy_keys"`
}

// readHostKeys reads host_keys.json; found false: there is none. One that
// cannot be read, or that is not a mapping of distinct slugs by project id,
// is an error (taken for none, a restored ledger would resolve against the
// joins of now; two projects on one slug would share a key tag, and with it
// their boxes and volumes).
func (s *Service) readHostKeys() (k hostKeys, found bool, err error) {
	b, err := os.ReadFile(filepath.Join(s.d.Config.Home, hostKeysFile))
	if os.IsNotExist(err) {
		return hostKeys{LegacyKeys: map[string]string{}}, false, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &k)
	}
	if err != nil {
		return k, false, err
	}
	if k.LegacyKeys == nil {
		k.LegacyKeys = map[string]string{}
	}
	idOf := map[string]string{}
	for id, slug := range k.LegacyKeys {
		if !ids.Valid(id) || !api.ValidSlug(slug) {
			return k, false, fmt.Errorf("legacy_keys %q: %q is not a project id and a slug", id, slug)
		}
		if other, dup := idOf[slug]; dup {
			return k, false, fmt.Errorf("legacy_keys: projects %s and %s both have slug %q", other, id, slug)
		}
		idOf[slug] = id
	}
	return k, true, nil
}

// hostMarked reports whether registry.db records the migration.
func (s *Service) hostMarked() (bool, error) {
	if s.d.Store == nil {
		return false, errors.New("no registry")
	}
	var v string
	err := s.d.Store.Registry().QueryRow(`SELECT value FROM run_host_keys WHERE key='migrated'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// hostMark records the migration in registry.db (after host_keys.json).
func (s *Service) hostMark() error {
	_, err := s.d.Store.Registry().Exec(`INSERT INTO run_host_keys (key, value) VALUES ('migrated', ?) ON CONFLICT (key) DO NOTHING`,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

// hostReady runs migrateHostKeys at start (Start) and again on every host
// call until it has finished; an error: not finished, and the caller
// changes nothing (a write before it would freeze the mapping without the
// old slugs). Slugs resolve only once teamlink has loaded the joins.
func (s *Service) hostReady() error {
	s.host.migrateMu.Lock()
	defer s.host.migrateMu.Unlock()
	if s.host.migrated {
		return nil
	}
	err := s.migrateHostKeys()
	s.host.migrated = err == nil
	s.host.done.Store(s.host.migrated)
	return err
}

// hostLog logs a host file message at most once a minute per message and
// arguments (the migration is retried on every host call, heartbeats
// included; each binding or share it drops is logged).
func (s *Service) hostLog(level slog.Level, msg string, args ...any) {
	s.host.logMu.Lock()
	defer s.host.logMu.Unlock()
	key := msg // and the arguments, but for errors (one may name a temporary file)
	for _, a := range args {
		if _, isErr := a.(error); !isErr {
			key += "\x00" + fmt.Sprint(a)
		}
	}
	if time.Since(s.host.logged[key]) < time.Minute {
		return
	}
	if s.host.logged == nil {
		s.host.logged = map[string]time.Time{}
	}
	s.host.logged[key] = time.Now()
	s.d.Log.Log(context.Background(), level, msg, args...)
}

// notMigrated is hostReady's refusal: msg is logged, and answered with hint
// (what the host's owner does).
func (s *Service) notMigrated(msg, hint string, args ...any) error {
	s.hostLog(slog.LevelError, "run targets: "+msg, args...)
	return api.Errorf(api.CodeUnavailable, "this host's bindings (%s) and handle ledger (%s) are not migrated to project ids: %s", HostFile, ledgerFile, msg).
		WithHint(hint)
}

// unresolved is an old ledger entry's owner whose slug the migration froze
// for no project: it matches no project (an id has no colon).
const unresolved = "slug:"

// oldOwner is an old ledger entry's owner (a slug) as frozen: the id of the
// project it meant, else unresolved. Call once migrated.
func (s *Service) oldOwner(slug string) string {
	if id := s.host.idOf[slug]; id != "" {
		return id
	}
	return unresolved + slug
}

// fromLegacy is an old bindings file's list with each share (a slug) as
// frozen: the project it meant, else dropped, and a binding left shared
// with none dropped. Call once migrated.
func (s *Service) fromLegacy(ts []computeapi.HostRunTarget) []computeapi.HostRunTarget {
	var kept []computeapi.HostRunTarget
	for _, t := range ts {
		var share []string
		for _, p := range t.Share {
			id := s.host.idOf[p]
			if id == "" {
				s.hostLog(slog.LevelWarn, "run targets: a share names a project this machine records no join to; dropped", "binding", t.Name, "project", p)
				continue
			}
			if !slices.Contains(share, id) {
				share = append(share, id)
			}
		}
		if len(share) == 0 {
			s.hostLog(slog.LevelWarn, "run targets: a machine-level binding is shared with no project this machine records; removed (share it again)", "binding", t.Name)
			s.host.cache.Delete(t.Name)
			continue
		}
		t.Share = share
		kept = append(kept, t)
	}
	return kept
}

// migrateHostKeys moves slug-keyed shares and ledger entries to project ids
// (see the top of this file); nil: nothing is left to move. What it finds,
// and does (the mark is hostMarked's; old files: a ledger without a version
// or a bindings file without a format line; new: with one):
//
//	key file  mark  files               does
//	none      no    none or old only    the first migration: freeze the joins of the start, write the key file, then the mark, then move the old files
//	none      no    any new             refuse: the key file was removed (restore it)
//	none      yes   none                write an empty key file (nothing is left that an old tag could reach)
//	none      yes   any                 refuse: the key file was removed (restore it, or remove both files and share again)
//	there     no    any                 set the mark (registry.db was restored or replaced), then as below
//	there     yes   any                 old files resolve only through the key file
func (s *Service) migrateHostKeys() error {
	// The joins the slugs mean are those of the first try that could read
	// them (at start): a retry after a failed write must not hand a slug to
	// a project joined since.
	if s.host.joins == nil && s.link != nil && s.link.Loaded() {
		s.host.joins = s.joinedProjects(false)
	}
	s.host.ledgerMu.Lock()
	defer s.host.ledgerMu.Unlock()
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	const checkHint = "the host's owner reads the daemon's log, then checks the file it names"
	led, err := s.ledgerLoad()
	if err != nil {
		return err // logged; every relayed call is refused until it is fixed
	}
	ts, legacyFile, present, err := s.readHostFile()
	if err != nil {
		return s.notMigrated("the machine's bindings cannot be read", checkHint, "file", HostFile, "err", err)
	}
	keys, found, err := s.readHostKeys()
	if err != nil {
		return s.notMigrated("the host's frozen key file cannot be read", "restore "+hostKeysFile+" from a backup; if there is none, remove it, "+
			HostFile+" and "+ledgerFile+" (projects lose the boxes this machine made for them) and share the bindings again", "file", hostKeysFile, "err", err)
	}
	marked, err := s.hostMarked()
	if err != nil {
		return s.notMigrated("the registry cannot be read", checkHint, "err", err)
	}
	oldLedger := led.found && !led.current
	oldFile := present && legacyFile
	newFiles := (led.found && led.current) || (present && !legacyFile)
	const restoreHint = "restore " + hostKeysFile + " to the daemon's home from the backup that holds it; if there is none, remove " +
		HostFile + " and " + ledgerFile + " there (projects lose the boxes this machine made for them) and share the bindings again"
	switch {
	case !found && marked && !led.found && !present:
		// No file is left that an old slug could resolve through: start
		// again empty (volumes a project left under its old tag are no
		// longer its; it has a new tag now).
	case !found && marked:
		return s.notMigrated("the frozen key file is missing, but the registry says this host was migrated (a backup restored over the files)", restoreHint, "file", hostKeysFile)
	case !found && newFiles:
		return s.notMigrated("the frozen key file is missing beside migrated files", restoreHint, "file", hostKeysFile)
	case !found:
		// The first migration (on every host, with or without old files, so
		// that a backup restored later resolves only through what is frozen
		// now): the slugs mean the projects this machine recorded at start.
		slugs := map[string]bool{}
		if oldFile {
			for _, t := range ts {
				for _, p := range t.Share {
					slugs[p] = true
				}
			}
		}
		if oldLedger {
			for _, e := range led.Handles {
				slugs[e.Project] = true
			}
		}
		if len(slugs) > 0 && s.host.joins == nil {
			// No teamlink, or it did not read the joins (it failed, or has
			// not started): committing now would drop every share. (An old
			// ledger naming no slug, empty say, moves on without them.)
			return s.notMigrated("the joined projects are not loaded", "the daemon's log says why the team joins did not load; restart the daemon once they do")
		}
		// No join recorded: every slug is dropped now, so none passes to a
		// project joined later under it.
		for slug := range slugs {
			if id := s.host.joins[slug]; id != "" {
				keys.LegacyKeys[id] = slug // joinedProjects names each slug once
			}
		}
	}
	if !found {
		b, _ := json.Marshal(keys)
		if err := atomicfile.WritePrivate(filepath.Join(s.d.Config.Home, hostKeysFile), b); err != nil {
			return s.notMigrated("the host's frozen key file cannot be written", checkHint, "file", hostKeysFile, "err", err)
		}
	}
	if !marked {
		if err := s.hostMark(); err != nil {
			return s.notMigrated("the registry cannot be written", checkHint, "err", err) // the key file is written: the next try resolves through it
		}
	}
	idOf := map[string]string{} // slug -> project id, only as frozen
	for id, slug := range keys.LegacyKeys {
		idOf[slug] = id
	}
	s.host.legacy, s.host.idOf = keys.LegacyKeys, idOf
	if oldLedger {
		for h, e := range led.Handles {
			e.Project = s.oldOwner(e.Project)
			led.Handles[h] = e
		}
		if err := s.ledgerWrite(led); err != nil {
			return s.notMigrated("the migrated handle ledger cannot be written", checkHint, "file", ledgerFile, "err", err)
		}
	}
	if oldFile {
		if err := s.saveHostBindings(s.fromLegacy(ts)); err != nil {
			return s.notMigrated("the migrated bindings cannot be written", checkHint, "file", HostFile, "err", err)
		}
	}
	return nil
}

// sharedWith reports whether t is shared with project (its id).
func sharedWith(t computeapi.HostRunTarget, project string) bool {
	return ids.Valid(project) && slices.Contains(t.Share, project)
}
