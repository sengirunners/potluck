package runtarget

// The host side of boxes across machines (SPEC 20 "Across machines"): the
// machine's own box bindings in $ZM_HOME/run_targets.toml, shared with team
// projects. Written only through the daemon by the machine's owner. A
// joined machine advertises each binding's describe in its heartbeat and
// runs the authority's run_target machine ops here (relay.go).
// Owned by run targets (zm-compute-runs).

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/atomicfile"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// HostFile is the machine-level bindings file inside the zm home.
const HostFile = "run_targets.toml"

const (
	// hostDescribeFresh: a binding's describe is reused this long before the
	// next beat asks the driver again (in the background: a slow driver never
	// stalls a heartbeat).
	hostDescribeFresh = 20 * time.Second
	// hostDescribeKeep: an older describe than this is not advertised.
	hostDescribeKeep = 2 * time.Minute
)

// hostState is the host's file lock and describe cache.
type hostState struct {
	ledgerMu sync.Mutex // host_handles.json
	mu       sync.Mutex // the file
	cache    sync.Map   // binding name -> *hostDescribe
	// hostReady (hostkeys.go): migrated once migrateHostKeys finished.
	migrateMu sync.Mutex
	migrated  bool
	joins     map[string]string    // slug -> id, as migrateHostKeys first read them
	legacy    map[string]string    // host_keys.json's legacy_keys, once migrated
	idOf      map[string]string    // legacy inverted: old slug -> project id
	done      atomic.Bool          // migrated, legacy and idOf set (ledgerLoad reads it)
	swapMu    sync.Mutex           // swap
	swap      swapCache            // this machine's swap, last read (swap.go)
	logMu     sync.Mutex           // hostLog
	logged    map[string]time.Time // by message
}

type hostDescribe struct {
	mu         sync.Mutex
	d          *computeapi.RunTargetDescribe // nil: the last ask failed
	at         time.Time
	refreshing bool
}

func (s *Service) hostPath() string { return filepath.Join(s.d.Config.Home, HostFile) }

// readHostFile reads the file as it is; legacy: from a build that keyed
// shares by slug; present: the file exists. Call with host.mu held.
func (s *Service) readHostFile() (ts []computeapi.HostRunTarget, legacy, present bool, err error) {
	b, err := os.ReadFile(s.hostPath())
	if os.IsNotExist(err) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	ts, legacy, err = rt.ParseHostRunTargets(string(b))
	return ts, legacy, true, err
}

// hostBindings reads the file (none when it does not exist). An old file
// (a backup restored after the migration) reads through what the migration
// froze (fromLegacy); before the migration it is as it is, slugs and all,
// for the owner's list (nothing else reads it before hostReady). Call with
// host.mu held.
func (s *Service) hostBindings() ([]computeapi.HostRunTarget, error) {
	ts, legacy, _, err := s.readHostFile()
	if err != nil || !legacy || !s.host.done.Load() {
		return ts, err
	}
	return s.fromLegacy(ts), nil
}

// saveHostBindings replaces the file, 0600, through a rename. Call with host.mu held.
func (s *Service) saveHostBindings(ts []computeapi.HostRunTarget) error {
	return atomicfile.WritePrivate(s.hostPath(), []byte(rt.EncodeHostRunTargets(ts)))
}

// hostBinding is the shared binding name serves for project (its id).
func (s *Service) hostBinding(project, name string) (computeapi.RunTarget, bool) {
	s.host.mu.Lock()
	ts, err := s.hostBindings()
	s.host.mu.Unlock()
	if err != nil {
		return computeapi.RunTarget{}, false
	}
	for _, t := range ts {
		if t.Name == name && sharedWith(t, project) {
			return t.RunTarget, true
		}
	}
	return computeapi.RunTarget{}, false
}

// bindHost is the owner's bind with req.Host: the binding goes in the
// machine's file, with every check a project binding has (describe must
// answer; the class must serve a box) and the share list.
func (s *Service) bindHost(ctx context.Context, req computeapi.BindRunTargetRequest) (any, error) {
	if err := s.hostReady(); err != nil {
		return nil, err
	}
	share, err := s.resolveShare(req.Share)
	if err != nil {
		return nil, err
	}
	h := computeapi.HostRunTarget{RunTarget: req.Target, Share: share}
	if err := rt.ValidateHostRunTarget(h); err != nil {
		return nil, err
	}
	b := h.RunTarget
	d, err := s.describeOf(ctx, b)
	if err != nil {
		return nil, err
	}
	if eff := stricter(b, d); !eff.UntrustedCapable() {
		return nil, api.Errorf(api.CodeBadRequest, "%s, so it cannot serve a box: a box runs whatever its holder sends", eff.CannotIsolate("potluck-driver-"+b.Driver))
	}
	var notes []string
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	ts, err := s.hostBindings()
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(ts, func(o computeapi.HostRunTarget) bool { return o.Name == b.Name })
	switch {
	case i >= 0 && !req.Replace:
		return nil, api.Errorf(api.CodeConflict, "run target %q is bound on this machine; pass replace to rebind it", b.Name)
	case i >= 0:
		ts[i] = h
	default:
		ts = append(ts, h)
	}
	if req.Setup {
		if notes, err = s.setup(ctx, b, d); err != nil {
			return nil, err
		}
	}
	if err := s.saveHostBindings(ts); err != nil {
		return nil, err
	}
	s.host.cache.Delete(b.Name)
	return computeapi.RunTargetView{RunTarget: shown(ctx, b), Effective: stricter(b, d), Describe: d, SetupNotes: notes}, nil
}

// unbindHost removes a machine-level binding (the owner's).
func (s *Service) unbindHost(name string) (any, error) {
	if err := s.hostReady(); err != nil {
		return nil, err
	}
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	ts, err := s.hostBindings()
	if err != nil {
		return nil, err
	}
	n := len(ts)
	ts = slices.DeleteFunc(ts, func(o computeapi.HostRunTarget) bool { return o.Name == name })
	if len(ts) == n {
		return nil, api.NotFound("run target", name)
	}
	if err := s.saveHostBindings(ts); err != nil {
		return nil, err
	}
	s.host.cache.Delete(name)
	return map[string]any{"removed": name}, nil
}

// listHost is the owner's view of the machine-level bindings. live also
// asks each binding's driver for its handles (list) and names those not
// ended: their boxes run for members of the projects the binding is shared
// with, whose rows are at those projects' authorities, not here.
func (s *Service) listHost(ctx context.Context, live bool) (any, error) {
	_ = s.hostReady() // a list is not refused: an unmigrated file is listed as it is
	s.host.mu.Lock()
	ts, err := s.hostBindings()
	s.host.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := computeapi.RunTargetList{Project: "", Targets: []computeapi.RunTargetView{}}
	for _, t := range ts {
		v := computeapi.RunTargetView{RunTarget: shown(ctx, t.RunTarget), Effective: stricter(t.RunTarget, nil), Shared: s.sharedNames(t.Share)}
		if d, err := s.describeOf(ctx, t.RunTarget); err != nil {
			v.Error = errText(err)
		} else {
			v.Describe, v.Effective = d, stricter(t.RunTarget, d)
		}
		if live {
			s.addLive(ctx, &v, t.RunTarget)
		}
		out.Targets = append(out.Targets, v)
	}
	return out, nil
}

// addLive sets v's Live (and Stopped among them) to b's handles its driver
// lists as not ended, or v's Error when the driver does not answer: unknown
// is not none, so a caller that needs the list refuses.
func (s *Service) addLive(ctx context.Context, v *computeapi.RunTargetView, b computeapi.RunTarget) {
	var l struct {
		Handles []struct {
			Handle string `json:"handle"`
			State  string `json:"state"`
		} `json:"handles"`
	}
	if err := s.callWithin(ctx, b, verbList, "", nil, &l, verbLimit); err != nil {
		v.Error = errText(err)
		return
	}
	v.Live = []string{}
	for _, h := range l.Handles {
		if handleOfBinding(h.Handle, b) && !terminal(h.State) {
			v.Live = append(v.Live, h.Handle)
			if h.State == computeapi.StateStopped {
				v.Stopped = append(v.Stopped, h.Handle)
			}
		}
	}
}

// HostAdverts answers the describe of each binding shared with the team
// project whose id is given (at most one driver call per binding per few
// seconds; a binding not yet described is left out).
func (s *Service) HostAdverts(ctx context.Context, project string) []computeapi.RunTargetAdvert {
	if s.hostReady() != nil {
		return nil
	}
	s.host.mu.Lock()
	ts, err := s.hostBindings()
	s.host.mu.Unlock()
	if err != nil {
		return nil
	}
	var out []computeapi.RunTargetAdvert
	live := map[string]bool{}
	swap := s.localSwap(ctx) // one reading for every binding of this beat (swap.go)
	for _, t := range ts {
		if !sharedWith(t, project) {
			continue
		}
		live[t.Name] = true
		d := s.hostDescribed(t.RunTarget)
		if d == nil {
			continue
		}
		eff := stricter(t.RunTarget, d)
		if !eff.UntrustedCapable() {
			continue // a box needs the untrusted-capable class, here as at bind
		}
		ad := *d
		// Describe may be cached across a rebind; advertise the current image name.
		ad.ImageName = t.Config["image_name"]
		ad.Images = slices.Clone(t.Images) // the binding's pins, never what a driver's describe says
		ad.Swap = swap                     // this machine's own reading, never a driver's word (swap.go)
		ad.Name, ad.Kind, ad.Isolation, ad.Credentials = t.Name, computeapi.RunTargetKindBox, eff.Isolation, eff.Credentials
		if lc := lifetimeCap(t.RunTarget, d); lc > 0 {
			ad.TTLMaxS = lc
		}
		out = append(out, computeapi.RunTargetAdvert{Binding: t.Name, Describe: ad})
	}
	s.host.cache.Range(func(k, _ any) bool {
		if n := k.(string); !live[n] && !s.hostShared(n) {
			s.host.cache.Delete(n) // unbound or unshared everywhere
		}
		return true
	})
	return out
}

// hostShared reports whether name is still in the file (any project).
func (s *Service) hostShared(name string) bool {
	s.host.mu.Lock()
	ts, err := s.hostBindings()
	s.host.mu.Unlock()
	return err == nil && slices.ContainsFunc(ts, func(t computeapi.HostRunTarget) bool { return t.Name == name })
}

// hostDescribed is b's describe as of the last ask, starting a background
// ask when it is older than hostDescribeFresh. nil: none yet, or too old.
func (s *Service) hostDescribed(b computeapi.RunTarget) *computeapi.RunTargetDescribe {
	v, _ := s.host.cache.LoadOrStore(b.Name, &hostDescribe{})
	hd := v.(*hostDescribe)
	hd.mu.Lock()
	defer hd.mu.Unlock()
	age := time.Since(hd.at)
	if hd.at.IsZero() || age > hostDescribeFresh {
		if !hd.refreshing {
			hd.refreshing = true
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), verbLimit+5*time.Second)
				defer cancel()
				d, err := s.describeOf(ctx, b)
				hd.mu.Lock()
				hd.refreshing, hd.at = false, time.Now()
				if err != nil {
					d = nil
				}
				hd.d = d
				hd.mu.Unlock()
			}()
		}
	}
	if hd.d == nil || age > hostDescribeKeep {
		return nil
	}
	d := *hd.d
	return &d
}

// HostCall runs one driver verb of binding for project (its id) exactly as a local
// call would (the driver's env allowlist, limits and error mapping): it
// refuses a binding that does not exist or is not shared with the project,
// as not_found. args and the answer are the driver's JSON.
func (s *Service) HostCall(ctx context.Context, project, binding, verb, id string, args json.RawMessage) (json.RawMessage, error) {
	if err := s.hostReady(); err != nil {
		return nil, err
	}
	b, ok := s.hostBinding(project, binding)
	if !ok {
		// Not bound, or not shared with this project: the same answer.
		return nil, api.NotFound("run target", binding)
	}
	if err := protocolKeys(args); err != nil { // before anything else reads them
		return nil, err
	}
	limit := verbLimit
	var a any
	if len(args) > 0 {
		a = args // sent to the driver as the authority built it
	}
	switch verb {
	case verbAcquire:
		limit = acquireLimit
		m, rid, err := relayedRequestID(verb, args)
		if err != nil {
			return nil, err
		}
		if _, err := s.ledgerRead(); err != nil {
			return nil, err // nothing is made that could not be recorded
		}
		key, err := s.projectKey(ctx, project)
		if err != nil {
			return nil, err
		}
		m["request_id"], _ = json.Marshal(hostKey(key, rid))
		// The authority's word for whose volumes they are holds only within
		// its project: one binding may serve several projects, and an owner
		// (or actor, the driver's fallback owner) is keyed by the project as
		// the request_id is, so no project's authority names another's
		// holder (volumes.go, zm-eakh.12.32.1).
		for _, k := range []string{"volume_owner", "actor"} {
			var v string
			if raw, ok := m[k]; ok && json.Unmarshal(raw, &v) == nil && v != "" {
				m[k], _ = json.Marshal(hostKey(key, v))
			}
		}
		// Whose project shares a volume cap is the host's word, never the authority's.
		m["volume_project"], _ = json.Marshal(hostKey(key, "volumes"))
		a = m
	case protocol.VerbProgress, protocol.VerbCancel: // acquires.go: by request_id, keyed by project as acquire's
		_, rid, err := relayedRequestID(verb, args)
		if err != nil {
			return nil, err
		}
		if verb == protocol.VerbCancel {
			caps := []quotaCap{{"project:" + project, cancelsRelayedMax}}
			if tag, _, ok := splitActorKey(rid); ok { // the actor's cap comes first, so its refusal and wait are the ones reported
				caps = append([]quotaCap{{"actor:" + project + "\x00" + tag, cancelsActorRelayedMax}}, caps...)
			}
			switch who, wait := s.cancels.takeAll(s.now(), caps...); {
			case who == "":
			case strings.HasPrefix(who, "actor:"):
				return nil, errCancelQuota(cancelsActorRelayedMax, "acquires relayed for one actor of a project", wait)
			default:
				return nil, errCancelQuota(cancelsRelayedMax, "acquires relayed for one project", wait)
			}
		}
		key, err := s.projectKey(ctx, project)
		if err != nil {
			return nil, err
		}
		a = map[string]string{"request_id": hostKey(key, rid)}
	case verbStatus, verbRelease, verbRenew, verbStop: // stop: persist.go
	case protocol.VerbSecrets: // boxsecrets.go: the authority checked the grant and this host's class
	case verbStart: // a start boots the box again
		limit = acquireLimit
	case verbExec:
		var x struct {
			TimeoutS int `json:"timeout_s"`
		}
		_ = json.Unmarshal(args, &x)
		limit = time.Duration(min(max(x.TimeoutS, 1), execMaxS)+30) * time.Second
	case protocol.VerbPort:
	case verbSpawn, verbSignal: // procs.go
	case verbInput: // pty.go
		limit = inputLimit
	case verbOutput:
		var m map[string]json.RawMessage
		var x struct {
			WaitS    int `json:"wait_s"`
			MaxBytes int `json:"max_bytes"`
		}
		if json.Unmarshal(args, &m) != nil || json.Unmarshal(args, &x) != nil {
			return nil, api.Errorf(api.CodeBadRequest, "a relayed output's args are an object")
		}
		limit = outputLimit(x.WaitS)
		if x.MaxBytes <= 0 || x.MaxBytes > relayOutputMax { // within the relay's answer; *_next says where to go on
			m["max_bytes"], _ = json.Marshal(min(cmp.Or(x.MaxBytes, outputDefault), relayOutputMax))
			a = m
		}
	case verbList:
		return s.hostList(ctx, project, b, args)
	default:
		return nil, api.Errorf(api.CodeBadRequest, "run target verb %q "+notRelayed, verb)
	}
	if verb != verbAcquire && verb != protocol.VerbProgress && verb != protocol.VerbCancel {
		held, err := s.ledgerHolds(project, id)
		if err != nil {
			return nil, err
		}
		if !handleOfBinding(id, b) || !held {
			// Another project's handle answers like a missing one (marked as
			// the driver's own not_found to a status is).
			return nil, rt.NoHandle(api.NotFound("handle", id))
		}
	}
	var out json.RawMessage
	err := s.callWithin(ctx, b, verb, id, a, &out, limit)
	if verb == protocol.VerbProgress || verb == protocol.VerbCancel {
		out, err = progressRelayed(verb, out, err) // acquires.go: only what the authority reads crosses the tunnel
	}
	if err != nil {
		// Scrubbed here, before the report leaves for the authority: over
		// the tunnel it crosses where Cloudflare reads it (SPEC 17.11
		// "Sealed secrets"), and a driver may quote what it was sent.
		var sent struct {
			Secrets   map[string]string `json:"secrets"`
			Unset     []string          `json:"unset"` // the secrets verb's (boxsecrets.go)
			JITConfig string            `json:"jit_config"`
		}
		_ = json.Unmarshal(args, &sent)
		for _, k := range sent.Unset {
			if sent.Secrets == nil {
				sent.Secrets = map[string]string{}
			}
			sent.Secrets[k] = ""
		}
		err = scrubJIT(scrubSecrets(err, sent.Secrets), sent.JITConfig)
	}
	if err == nil && verb == verbAcquire {
		var h struct {
			Handle string `json:"handle"`
		}
		if json.Unmarshal(out, &h) == nil && handleOfBinding(h.Handle, b) {
			if had, lerr := s.ledgerRecord(project, h.Handle); errors.Is(lerr, errLedgerOther) {
				// Another project's box (made before keys carried the
				// project): never this caller's, and never ended for it.
				return nil, api.Errorf(api.CodeConflict, "request_id answers a box this host made for another project")
			} else if lerr != nil {
				if !had { // ours alone (the key carries the project), and unreachable unrecorded: end it
					_ = s.callWithin(ctx, b, verbRelease, h.Handle, nil, nil, verbLimit)
				}
				return nil, api.Errorf(api.CodeInternal, "the host could not record the handle: %v", lerr)
			}
		}
	} else if err == nil {
		s.ledgerTouch(project, id)
	}
	return out, hostErr(err)
}

// hostList is list for the authority's reconcile: only the handle whose
// request_id the authority names (one binding may serve several projects).
// A handle of b's whose request_id is this project's key for it and that
// the ledger does not hold is adopted: the driver made it for this project
// (the key carries the project) in an acquire that failed or timed out
// before HostCall could record it. Recorded here, it is this project's for
// status and release too, and the authority's reconcile counts it, so a
// retry answers it instead of making a second box.
func (s *Service) hostList(ctx context.Context, project string, b computeapi.RunTarget, args json.RawMessage) (json.RawMessage, error) {
	var want struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(args, &want) != nil || want.RequestID == "" {
		return nil, api.Errorf(api.CodeBadRequest, "list across machines names a request_id")
	}
	var l struct {
		Handles []json.RawMessage `json:"handles"`
	}
	if err := s.callWithin(ctx, b, verbList, "", nil, &l, verbLimit); err != nil {
		return nil, hostErr(err)
	}
	led, err := s.ledgerRead()
	if err != nil {
		return nil, err
	}
	key, err := s.projectKey(ctx, project)
	if err != nil {
		return nil, err
	}
	keep := []json.RawMessage{}
	for _, h := range l.Handles {
		var x map[string]json.RawMessage
		var handle, rid string
		if json.Unmarshal(h, &x) != nil || json.Unmarshal(x["handle"], &handle) != nil || json.Unmarshal(x["request_id"], &rid) != nil {
			continue
		}
		if rid != hostKey(key, want.RequestID) || !handleOfBinding(handle, b) {
			continue
		}
		if e, ok := led.Handles[handle]; !ok {
			if _, err := s.ledgerRecord(project, handle); errors.Is(err, errLedgerOther) {
				continue // recorded for another project since the read
			} else if err != nil {
				return nil, err // unrecorded, never reported: the authority keeps its request
			}
		} else if e.Project != project {
			continue
		}
		x["request_id"], _ = json.Marshal(want.RequestID) // the key as the authority knows it
		out, _ := json.Marshal(x)
		keep = append(keep, out)
	}
	return json.Marshal(map[string]any{"handles": keep})
}

// protocolKeys refuses args whose top-level keys are not the protocol's own
// spelling (protocolKey): a driver decoding them case-insensitively (Go's
// encoding/json folds case and look-alikes) would take VOLUME_OWNER,
// Request_ID or requeſt_id as the keys HostCall rewrites, and so an authority
// could name another project's holder, key or project (zm-eakh.12.32.1
// review). It also refuses a top-level *_file key: see the loop.
func protocolKeys(args json.RawMessage) error {
	if len(bytes.TrimSpace(args)) == 0 || bytes.TrimSpace(args)[0] != '{' {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return api.Errorf(api.CodeBadRequest, "relayed args are not a JSON object: %v", err)
	}
	for k := range m {
		if !protocolKey.MatchString(k) {
			return api.Errorf(api.CodeBadRequest, "relayed arg %q is not a protocol key (SPEC 20's are lowercase ASCII)", k)
		}
		if strings.HasSuffix(k, "_file") {
			// A file on this machine is named only by its owner, in the
			// binding's config (a credential reaches a driver only that way):
			// an authority naming one would have the driver read it for its box.
			return api.Errorf(api.CodeBadRequest, "relayed arg %q names a file: only this machine's own binding config does", k)
		}
	}
	return nil
}

// protocolKey is SPEC 20's spelling of an arg key: ASCII lowercase, digits and
// _. Go's decoder also folds look-alikes (ſ to s, ı to i, K to k), which a
// lowercase check alone lets through.
var protocolKey = regexp.MustCompile(`^[a-z0-9_]+$`)

// handleOfBinding reports whether id is a handle of b's box.
func handleOfBinding(id string, b computeapi.RunTarget) bool {
	return len(id) > len(b.Kind)+len(b.Name)+2 && id[:len(b.Kind)+len(b.Name)+2] == b.Kind+":"+b.Name+"/"
}

// hostErr marks a driver timeout so it travels to the authority as one.
func hostErr(err error) error {
	if err != nil && errors.Is(err, errTimeout) {
		return api.Errorf(rt.CodeDriverTimeout, "%v", err)
	}
	return err
}

// The host's handle ledger (SPEC 20 "Across machines"): which project's
// authority (by project id) acquired each handle this machine's drivers
// issued through a relay, in $ZM_HOME/host_handles.json (0600). Any other
// project's verbs on a handle answer not_found. An entry stays until nothing
// touched it for ledgerKeep, far past the 24 h an ended handle is kept.
const (
	ledgerFile    = "host_handles.json"
	ledgerKeep    = 30 * 24 * time.Hour
	ledgerVersion = 2
)

type ledgerEntry struct {
	Project string    `json:"project"`
	At      time.Time `json:"at"`
}

// ledger is host_handles.json. Version 1 (from builds that keyed it by the
// project's slug) is the bare Handles map; migrateHostKeys moves it on.
type ledger struct {
	Version int                    `json:"version"`
	Handles map[string]ledgerEntry `json:"handles"`
	current bool                   // read in this version (or none yet)
	found   bool                   // read from the file (not a new one)
}

// errLedgerOther: the handle is recorded for another project.
var errLedgerOther = errors.New("handle held by another project")

// ledgerLoad reads the ledger (empty when there is none). One that cannot be
// read or parsed is an error: taken for empty, the next write would orphan
// every other project's handles. Call with host.ledgerMu held.
func (s *Service) ledgerLoad() (*ledger, error) {
	led := &ledger{Version: ledgerVersion, Handles: map[string]ledgerEntry{}, current: true}
	b, err := os.ReadFile(filepath.Join(s.d.Config.Home, ledgerFile))
	if os.IsNotExist(err) {
		return led, nil
	}
	if err == nil {
		led.found = true
		var top map[string]json.RawMessage
		if err = json.Unmarshal(b, &top); err == nil {
			if _, v2 := top["version"]; v2 {
				err = json.Unmarshal(b, led)
				if err == nil && led.Version != ledgerVersion {
					err = fmt.Errorf("version %d is not this build's (%d)", led.Version, ledgerVersion)
				}
			} else { // version 1: handle -> entry (a handle always has a colon, so never "version")
				err = json.Unmarshal(b, &led.Handles)
				// Once migrated, an old ledger is a restored backup: its slugs
				// mean what host_keys.json froze, and nothing else.
				led.current = s.host.done.Load()
				for h, e := range led.Handles {
					if led.current {
						e.Project = s.oldOwner(e.Project)
						led.Handles[h] = e
					}
				}
			}
			if led.Handles == nil {
				led.Handles = map[string]ledgerEntry{}
			}
		}
	}
	if err != nil {
		s.hostLog(slog.LevelError, "run targets: the host's handle ledger cannot be read; relayed calls are refused", "file", ledgerFile, "err", err)
		return nil, api.Errorf(api.CodeUnavailable, "this host's handle ledger (%s) cannot be read", ledgerFile).
			WithHint("the host's owner checks or restores $ZM_HOME/" + ledgerFile + " (a copy this build wrote: it also holds the projects' old key tags)")
	}
	return led, nil
}

// ledgerRead is ledgerLoad under the ledger's lock.
func (s *Service) ledgerRead() (*ledger, error) {
	s.host.ledgerMu.Lock()
	defer s.host.ledgerMu.Unlock()
	return s.ledgerLoad()
}

func (s *Service) ledgerWrite(led *ledger) error {
	for h, e := range led.Handles {
		if time.Since(e.At) > ledgerKeep {
			delete(led.Handles, h)
		}
	}
	led.Version = ledgerVersion
	b, _ := json.Marshal(led)
	return atomicfile.WritePrivate(filepath.Join(s.d.Config.Home, ledgerFile), b)
}

func (s *Service) ledgerHolds(project, handle string) (bool, error) {
	led, err := s.ledgerRead()
	if err != nil {
		return false, err
	}
	e, ok := led.Handles[handle]
	return ok && e.Project == project, nil
}

// ledgerRecord records handle for project; had: it was already its.
func (s *Service) ledgerRecord(project, handle string) (had bool, err error) {
	s.host.ledgerMu.Lock()
	defer s.host.ledgerMu.Unlock()
	led, err := s.ledgerLoad()
	if err != nil {
		return false, err
	}
	if !led.current {
		return false, api.Errorf(api.CodeUnavailable, "this host's handle ledger (%s) is not migrated yet", ledgerFile)
	}
	e, ok := led.Handles[handle]
	if ok && e.Project != project {
		return false, errLedgerOther
	}
	led.Handles[handle] = ledgerEntry{Project: project, At: time.Now()}
	return ok, s.ledgerWrite(led)
}

func (s *Service) ledgerTouch(project, handle string) {
	s.host.ledgerMu.Lock()
	defer s.host.ledgerMu.Unlock()
	led, err := s.ledgerLoad()
	if err != nil || !led.current {
		return
	}
	if e, ok := led.Handles[handle]; ok && e.Project == project && time.Since(e.At) > time.Hour {
		led.Handles[handle] = ledgerEntry{Project: project, At: time.Now()}
		_ = s.ledgerWrite(led)
	}
}

// relayedRequestID reads the request_id a relayed acquire, progress or
// cancel must name, with the rest of its arguments.
func relayedRequestID(verb string, args json.RawMessage) (map[string]json.RawMessage, string, error) {
	var m map[string]json.RawMessage
	var rid string
	if json.Unmarshal(args, &m) != nil || json.Unmarshal(m["request_id"], &rid) != nil || rid == "" {
		return nil, "", api.Errorf(api.CodeBadRequest, "a relayed %s names a request_id", verb)
	}
	return m, rid, nil
}
