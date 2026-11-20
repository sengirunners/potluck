package vmhost

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	kitconfig "github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/protocol"
)

// kind=box (SPEC 20 "Boxes"): a VM kept up for its lease, which its holder
// drives with exec (exec.go). One size per binding; images are owner-set and
// pinned by digest, because a renter is told which image_digest it booted: a
// local VM has no honest digest, so a box image is an OCI reference ending
// in @sha256:<64 hex>, which Tart and the OCI layer both clone directly.
var pinnedRe = regexp.MustCompile(`^[^\s,\x00-\x1f\x7f]+@sha256:[0-9a-f]{64}$`)

func (c *config) parseBox(m map[string]string, bad *[]string) {
	if c.Size = m["size"]; !regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`).MatchString(c.Size) {
		*bad = append(*bad, "a box needs size, a short lowercase name (describe.size.name)")
	}
	for _, s := range strings.Split(m["images"], ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.Images = append(c.Images, s)
		}
	}
	if len(c.Images) == 0 {
		*bad = append(*bad, "a box needs images: OCI references pinned by digest (ref@sha256:...), the first the default")
	}
	for _, s := range c.Images {
		if !pinnedRe.MatchString(s) || len(s) > protocol.MaxRunnerImage { // describe.runners names the image as written
			*bad = append(*bad, fmt.Sprintf("box image %q is not pinned by digest (ref@sha256:<64 hex>, at most %d bytes)", s, protocol.MaxRunnerImage))
		}
	}
	if len(c.Images) > 0 {
		c.Image = c.Images[0]
	}
	if c.CPU <= 0 || c.Memory <= 0 {
		*bad = append(*bad, "a box needs cpu and memory: describe.size states them")
	}
	if c.JITRunnerDir = m["runner_dir"]; strings.ContainsAny(c.JITRunnerDir, "\"'`$\\\n") {
		*bad = append(*bad, "runner_dir must be a plain path")
	}
	if c.JITRunnerDir != "" && c.VM == "oci" && c.OCINetwork == "none" { // zm-eakh.12.24.6
		*bad = append(*bad, "runner_dir needs a network: a GitHub runner must reach GitHub, and oci_network is none ("+kitconfig.PotluckCommand("doctor")+" prints the root step that makes "+kitconfig.Program+"'s filtered network)")
	}
}

// digestOf is the digest an image pins.
func digestOf(image string) string {
	_, d, _ := strings.Cut(image, "@")
	return d
}

// jitRe is what an encoded JIT runner config looks like (base64), bounded.
var jitRe = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{16,}$`)

const jitMax = 64 << 10

// jitStartWindow is how long a runner must stay up before acquire answers.
const jitStartWindow = 5 * time.Second

// jitLaunch starts the GitHub runner in the guest with the config read from
// stdin, so it is never in argv (the host's ps would show it) or on disk.
// With a guard ($2 the repository, lowercase; $3 the events, space-separated,
// none for any; the driver checked both), the runner gets a job-started hook
// (ACTIONS_RUNNER_HOOK_JOB_STARTED, outside the runner's directory, as GitHub
// asks) that stops a job the broker would not serve: of another repository,
// or (with events) started by another event or from a fork (a pull_request*
// event whose payload's head repository is not the repository; its fork flag
// is not read, as a repository that is itself a fork has it on its own
// branches). It is JavaScript, which the runner runs with its own node, so the
// event payload is parsed, not grepped. The runner runs the hook once a job
// is assigned, after "Set up job" and before any other step, and waits for
// it; a hook's non-zero exit only marks the job failed (actions/runner: its
// if: always() steps and action pre: scripts still run), so on a refusal the
// hook SIGKILLs its parent first (Runner.Worker: the runner starts the hook's
// node itself), so no step can start, then the rest of its ancestors up to
// run.sh, else run.sh's process group (found through /proc, else ps); with
// $4 "off" (a VM layer) it then powers the guest off where it may (root, or
// sudo -n), and the box ends. An image where the launch cannot find its
// process group (no /proc, no ps -p), or whose runner .env names a hook of
// its own, starts nothing.
//
// $5, when it is a directory in the guest, is the owner's read-only cache
// (ociCacheDir, zm-eakh.12.43). Its tools, laid out as GitHub's hosted tool
// cache is (tools/<tool>/<version>/<arch>/ with <arch>.complete beside it),
// are linked into the runner's tool directory (RUNNER_TOOL_CACHE: the
// image's, else the runner's default _work/_tool, said aloud so the links
// are where the runner looks), so actions/setup-go and its kin find a cached
// version and download nothing. Only an <arch> the cache marks complete is
// linked, the directory and its marker each by a link of their own in a
// <version> directory the box makes, so nothing the job may write is
// read-only: a tool the cache lacks or holds half of (a fill still running)
// installs in the box as ever (the action removes the directory and the
// marker, then writes both). Its goproxy/ is in GOPROXY already: the box was
// created with it (cacheArgs). Nothing here can stop the runner from
// starting.
//
// With a run ($6 GITHUB_RUN_ID, $7 GITHUB_WORKFLOW_REF, $8 GITHUB_REF, $9 the
// hold in seconds, ${10} the box's secrets directory, protocol.BoxSecretsDir;
// all empty for none), the guard also stops a job of another run, workflow
// (compared without case, as GitHub's owner and name are) or ref, then holds
// the job its rules serve until the door's ready marker (${10}/POTLUCK_READY,
// protocol.SecretNameReady) holds the run's id, for at most $9 seconds, and
// stops it as it stops a refused one if the marker never comes. A job of
// another run (GitHub gives a runner any queued job of its repository with
// its labels) that the strict guard serves otherwise (${11} not "private",
// its event among $3, not a fork's) is held the same way for a re-guard
// marker (protocol.ReguardMarker): the door writes "<run> <workflow ref>
// <ref>" naming that job's own run only after the broker admitted it and
// issued its own credentials alone, so it passes on its own GITHUB_RUN_ID,
// GITHUB_WORKFLOW_REF (without case) and GITHUB_REF and nothing else; no
// marker in the hold stops it as any refusal (killed: none of its steps run,
// if: always() ones included). The values
// go into the hook as they are: the driver let only a run that
// protocol.JITRun.Check passed through, whose characters cannot leave a JS
// string or start a shell expansion. The runner, and so its steps, get the
// directory as POTLUCK_SECRETS and ZM_SECRETS.
//
// ${11} "private" (jit_guard.open_if_private, with events) also serves a job
// the events refuse (another event's run, a fork's) when the job's own event
// payload says the repository is private: repository.private true and
// repository.visibility "private". The broker decided at the mint that the
// repository was private; this checks it again at the job's start, as the
// run's payload says it (fixed when GitHub created the run), so a run
// created after the repository was made public (or internal) is stopped
// before its first step. A payload that does not say both is not private:
// stricter than ghapp.Public, which takes private with no visibility as
// private because the broker reads GitHub's full answer again.
const jitLaunch = `IFS= read -r c; case "$1" in /*) cd "$1" ;; *) cd "$HOME/$1" ;; esac || exit 125
[ -x ./run.sh ] || { echo "potluck-driver-tart: no GitHub runner at $1 in this image" >&2; exit 127; }
if [ -n "$6" ] && { [ -z "$2" ] || [ -z "${10}" ]; }; then
  echo "potluck-driver-tart: a runner held to one run needs its guard and the secrets directory" >&2; exit 125
fi
if [ -n "$2" ]; then
  if grep -q ACTIONS_RUNNER_HOOK_JOB_STARTED .env 2>/dev/null; then
    echo "potluck-driver-tart: the image's runner .env sets its own job-started hook" >&2; exit 125
  fi
  pg=
  if [ -r /proc/$$/stat ]; then
    IFS= read -r st </proc/$$/stat; st=${st##*) }; st=${st#* }; st=${st#* }; pg=${st%% *}
  fi
  [ -n "$pg" ] || pg=$(ps -o pgid= -p $$ 2>/dev/null) || pg=
  pg=$(echo $pg)
  case "$pg" in ''|*[!0-9]*)
    echo "potluck-driver-tart: cannot find the runner's process group (no /proc and no ps -p in this image), so a refused job could not be stopped: no runner" >&2; exit 125 ;;
  esac
  h=$(mktemp -d) || exit 125
  cat >"$h/zm-job-started.js" <<ZMHOOK || exit 125
// zm: this runner serves only a job the broker would serve; any other is stopped here, before its first step.
const fs = require('fs'), cp = require('child_process'), env = process.env;
const repo = '$2', events = '$3'.split(' ').filter(Boolean), root = $$, group = '$pg', off = '$4' === 'off';
const run = '$6', workflow = '$7'.toLowerCase(), ref = '$8', hold = Number('$9') || 0, ready = '${10}/POTLUCK_READY';
const openIfPrivate = '${11}' === 'private';
function payload() { try { return JSON.parse(fs.readFileSync(env.GITHUB_EVENT_PATH, 'utf8')) || {}; } catch (e) { return {}; } }
// The job's own event payload says the repository is private: private true and visibility "private" (an internal
// repository, one whose payload leaves either unsaid, or no payload, is not).
function privateNow() {
  const r = payload().repository || {};
  return r.private === true && r.visibility === 'private';
}
function refused() {
  if (String(env.GITHUB_REPOSITORY || '').toLowerCase() !== repo) return 'of another repository';
  if (events.length === 0) return '';
  const why = untrusted();
  if (why !== '' && openIfPrivate && privateNow()) return '';
  return why;
}
function untrusted() {
  const ev = String(env.GITHUB_EVENT_NAME || '');
  if (!events.includes(ev)) return 'of a ' + (ev || 'nameless') + ' run';
  if (ev.startsWith('pull_request')) {
    let head = null;
    try { head = ((payload().pull_request || {}).head || {}).repo || null; } catch (e) {}
    if (!head || String(head.full_name).toLowerCase() !== repo) return 'of a run from a fork';
  }
  return '';
}
// A runner held to one run (jit_guard.run) serves that run's job alone: the door mints its secrets for it.
function otherRun() {
  if (run === '') return '';
  if (String(env.GITHUB_RUN_ID || '') !== run) return 'of another run';
  if (String(env.GITHUB_WORKFLOW_REF || '').toLowerCase() !== workflow) return 'of another workflow';
  if (String(env.GITHUB_REF || '') !== ref) return 'of another ref';
  return '';
}
// held waits for the door's ready marker to be want, for at most hold seconds.
function held(want) {
  const nap = new Int32Array(new SharedArrayBuffer(4)), until = Date.now() + hold * 1000;
  for (;;) {
    let v = null;
    try { v = fs.readFileSync(ready, 'utf8').trim(); } catch (e) {}
    if (v === want) return true;
    const left = until - Date.now();
    if (left <= 0) return false;
    Atomics.wait(nap, 0, 0, Math.min(250, left));
  }
}
// The re-guard marker this job passes on (protocol.ReguardMarker): its own run, workflow ref (lower case) and ref; ''
// for a job whose values could not be one (the door writes only values protocol.JITRun.Check passed).
function reguard() {
  const id = String(env.GITHUB_RUN_ID || ''), wf = String(env.GITHUB_WORKFLOW_REF || '').toLowerCase(), r = String(env.GITHUB_REF || '');
  if (!/^[1-9][0-9]{0,19}$/.test(id) || !/^[a-z0-9._/@-]+$/.test(wf) || !/^refs\/heads\/[A-Za-z0-9._/-]+$/.test(r)) return '';
  return id + ' ' + wf + ' ' + r;
}
const why = refused() || otherRun();
let stop = '';
// A job of another run that the strict guard serves otherwise (never an open guard's widening): held for the broker
// to re-guard this box to its own run (runners/jobsecrets.go), else stopped as any refusal.
const other = why === 'of another run' && !openIfPrivate && untrusted() === '' ? reguard() : '';
if (other !== '') {
  if (!held(other)) stop = 'zm: this runner was minted for run ' + run + '; this job is of another run, which the Potluck broker did not take this box over for in ' + hold + 's: stopped before its first step';
} else if (why !== '') stop = 'zm: this runner serves only jobs the broker would serve; this job is ' + why + ': stopped before its first step';
else if (run !== '' && !held(run)) stop = 'zm: this job was held for its secrets from the Potluck secrets door, which did not deliver them in ' + hold + 's: stopped before its first step';
if (stop === '') process.exit(0);
console.error(stop);
const ps = (args) => { try { return cp.execFileSync('ps', args, { stdio: ['ignore', 'pipe', 'ignore'] }).toString(); } catch (e) { return ''; } };
const stat = (pid) => { try { const s = fs.readFileSync('/proc/' + pid + '/stat', 'utf8'); return s.slice(s.lastIndexOf(')') + 2).split(' '); } catch (e) { return null; } };
const parentOf = (pid) => { const f = stat(pid); return f ? Number(f[1]) : parseInt(ps(['-o', 'ppid=', '-p', String(pid)]).trim(), 10) || 0; };
const kill = (pid) => { try { process.kill(pid, 'SIGKILL'); } catch (e) {} };
// The runner starts this file's node itself, so the parent is Runner.Worker, which waits for it: killed
// first, no step can start, whatever else fails below.
const worker = process.ppid, above = parentOf(worker);
kill(worker);
let up = [worker], p = above;
while (p > 1 && !up.includes(root) && !up.includes(p)) { up.push(p); p = parentOf(p); }
let rest = up.includes(root) ? up : [];
if (!rest.length) {
  try {
    rest = fs.readdirSync('/proc').filter((d) => String(Number(d)) === d).filter((d) => { const f = stat(d); return f && f[2] === group; }).map(Number);
  } catch (e) {
    rest = ps(['-A', '-o', 'pid=', '-o', 'pgid=']).split('\n').map((l) => l.trim().split(' ').filter(Boolean))
      .filter((f) => f[1] === group).map((f) => Number(f[0]));
  }
}
for (const v of rest) { if (v !== process.pid) kill(v); }
if (off) {
  const o = { stdio: 'ignore', env: Object.assign({}, env, { PATH: (env.PATH || '') + ':/sbin:/usr/sbin' }) };
  try { cp.execFileSync(process.getuid() === 0 ? 'shutdown' : 'sudo', process.getuid() === 0 ? ['-h', 'now'] : ['-n', 'shutdown', '-h', 'now'], o); } catch (e) {}
}
process.exit(1);
ZMHOOK
  export ACTIONS_RUNNER_HOOK_JOB_STARTED="$h/zm-job-started.js"
fi
if [ -n "$6" ]; then
  export POTLUCK_SECRETS="${10}" ZM_SECRETS="${10}"
fi
if [ -n "$5" ] && [ -d "$5/tools" ]; then
  t=${RUNNER_TOOL_CACHE:-$PWD/_work/_tool}
  for m in "$5"/tools/*/*/*.complete; do
    a=${m%.complete}; [ -f "$m" ] && [ -d "$a" ] || continue
    v=${a%/*}; n=${v%/*}; d=$t/${n##*/}/${v##*/}
    [ -e "$d/${a##*/}" ] || [ -e "$d/${m##*/}" ] && continue
    mkdir -p "$d" 2>/dev/null && ln -s "$a" "$d/${a##*/}" 2>/dev/null && ln -s "$m" "$d/${m##*/}" 2>/dev/null
  done
  export RUNNER_TOOL_CACHE="$t"
fi
exec ./run.sh --jitconfig "$c"`

// jitCache is jitLaunch's $5: where an oci box has the owner's cache, if the
// owner made one (the launch checks the directory).
func jitCache(c config) string {
	if c.VM == "oci" {
		return ociCacheDir
	}
	return ""
}

// ghRepoRe is a jit_guard's repo: OWNER/REPO.
var ghRepoRe = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// jitEvents says whether events are GitHub Actions event names (jit_guard).
func jitEvents(events []string) bool {
	for _, e := range events {
		if !jitEventRe.MatchString(e) {
			return false
		}
	}
	return len(events) <= 32
}

var jitEventRe = regexp.MustCompile(`^[a-z_]{1,64}$`)

// jitStop is jitLaunch's $4: "off" where the guest is a VM of its own (Tart,
// Incus), so a stopped job's hook also powers it off. Never a container (oci)
// or the test layer, whose "guest" is the host.
func jitStop(c config) string {
	if c.VM == "tart" || c.VM == "incus" {
		return "off"
	}
	return ""
}

// pinned finds the binding's image a caller named: by name (what zm sends,
// SPEC 20: the part before @sha256:) or by its full pinned reference.
func (c config) pinned(name string) (string, bool) {
	for _, im := range c.Images {
		if i := strings.LastIndex(im, "@"); im == name || i > 0 && im[:i] == name {
			return im, true
		}
	}
	return "", false
}
