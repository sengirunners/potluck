package conformance

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ProbeScript is the script a run target's test commit carries, named with
// --script. Exit codes are how a run reports back: they are the one channel
// SPEC 20 guarantees (log is optional, and its form is not specified), so the
// driver must pass the script's exit code through.
const ProbeScript = `#!/bin/sh
# zm-run-conformance's probe (SPEC 20). Commit it; name it with --script.
#   beat FILE      append to FILE and, from a background child, to FILE.child, until killed
#   sleep SECONDS  run that long
#   exit CODE      exit CODE
#   head SHA       exit 0 if HEAD is SHA and the checkout is clean; 3 if not SHA; 4 if dirty
#   net allow H:P  exit 0 if a TCP connection to H:P opens, 7 if not
#   net forbid H:P exit 0 if it does not open, 5 if it does
#   net self PORT  exit 0 if this guest's tools see a port they listen on as open and port 1 as closed, 66 if not
#   net serve PORT listen on PORT until killed (66 if it cannot)
#   net peers PORT exit 0 if no other machine on this run's IPv4 /24 or IPv6 link-local neighbours that answer
#                  ping accepts on PORT, 6 if one does
#   net resolve NAME PORT  exit 0 if NAME resolves (getent hosts), 8 if not; without getent, 0 if NAME:PORT opens, 7 if not
#   (net exits 65 when the guest lacks the tools: nc or bash, ip, ping)
tcp() {
	# macOS's nc -w bounds idle time, not the connect: a dropped SYN would wait out the
	# kernel's 75 s. Its -G bounds the connect and -z closes once it opens, so a service
	# that keeps talking still reads open. Never a kill: a killed nc may have connected.
	if [ "$ncg" = 1 ]; then nc -z -G 3 -w 3 "$1" "$2" </dev/null >/dev/null 2>&1; return; fi
	# Elsewhere no -z (busybox nc lacks it): -w bounds the connect, and with stdin at EOF
	# every nc connects and exits 0, or fails.
	if command -v nc >/dev/null 2>&1; then nc -w 3 "$1" "$2" </dev/null >/dev/null 2>&1; return; fi
	if command -v bash >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1; then
		timeout 3 bash -c 'exec 3<>"/dev/tcp/$0/$1"' "$1" "$2" 2>/dev/null; return
	fi
	exit 65
}
hostport() { h=${1%:*}; p=${1##*:}; h=${h#[}; h=${h%]}; }
# listening PORT: start one listener with whichever -l form this nc takes, see it
# answer, and set form for the next ones; exit 66 if neither form listens.
listening() {
	command -v nc >/dev/null 2>&1 || exit 65
	for form in p n; do
		if [ "$form" = p ]; then nc -l -p "$1" </dev/null >/dev/null 2>&1 & else nc -l "$1" </dev/null >/dev/null 2>&1 & fi
		lp=$!; sleep 0.5
		if kill -0 "$lp" 2>/dev/null && tcp 127.0.0.1 "$1"; then wait "$lp" 2>/dev/null; return 0; fi
		kill "$lp" 2>/dev/null
	done
	exit 66
}
case "$1" in
net)
	# Checked here, not in tcp: a tcp in a background job exits only its subshell.
	command -v nc >/dev/null 2>&1 || { command -v bash >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1; } || exit 65
	# ncg=1: this nc has macOS's -G conntimo. Set once here so the scan's background jobs see it.
	ncg=; if command -v nc >/dev/null 2>&1 && nc -h 2>&1 | grep -q conntimo; then ncg=1; fi
	case "$2" in
	allow) hostport "$3"; tcp "$h" "$p" || exit 7 ;;
	forbid) hostport "$3"; if tcp "$h" "$p"; then exit 5; fi ;;
	resolve)
		if command -v getent >/dev/null 2>&1; then getent hosts "$3" >/dev/null 2>&1 || exit 8
		else tcp "$3" "$4" || exit 7; fi ;;
	self)
		listening "$3"
		if tcp 127.0.0.1 1; then exit 66; fi ;;
	serve)
		listening "$3"
		while :; do
			if [ "$form" = p ]; then nc -l -p "$3" </dev/null >/dev/null 2>&1 || sleep 0.2
			else nc -l "$3" </dev/null >/dev/null 2>&1 || sleep 0.2; fi
		done ;;
	peers)
		command -v ip >/dev/null 2>&1 || exit 65
		hits=$(mktemp); port=$3
		for cidr in $(ip -4 -o addr show scope global | awk '{print $4}'); do
			self=${cidr%/*}; base=${self%.*}; i=1
			while [ "$i" -le 254 ]; do
				[ "$base.$i" = "$self" ] || { tcp "$base.$i" "$port" && echo "$base.$i" >>"$hits"; } &
				i=$((i + 1))
			done
		done
		wait
		if command -v ping >/dev/null 2>&1; then
			for ifc in $(ip -6 -o addr show scope link | awk '{print $2}' | sort -u); do
				for a in $(ping -6 -c 2 -w 3 "ff02::1%$ifc" 2>/dev/null | sed -n 's/.*from \([0-9a-f:]*\).*/\1/p' | sort -u); do
					ip -6 -o addr show dev "$ifc" | grep -q " $a/" && continue
					{ tcp "$a%$ifc" "$port" && echo "$a" >>"$hits"; } &
				done
			done
			wait
		fi
		if [ -s "$hits" ]; then cat "$hits"; rm -f "$hits"; exit 6; fi
		rm -f "$hits" ;;
	*) exit 64 ;;
	esac
	;;
beat)
	dir=${2%/*}
	loop() {
		n=0
		while [ -d "$dir" ] && [ "$n" -lt 3000 ]; do
			echo "$$" >>"$1"
			n=$((n + 1))
			sleep 0.2
		done
	}
	loop "$2.child" &
	loop "$2"
	;;
sleep) exec sleep "$2" ;;
exit) exit "$2" ;;
head)
	[ "$(git rev-parse HEAD)" = "$2" ] || exit 3
	[ -z "$(git status --porcelain)" ] || exit 4
	;;
*) exit 64 ;;
esac
`

const (
	exitWrongHead = 3
	exitDirty     = 4
	exitExpired   = 124
	// stillFor is how long a heartbeat must be silent to count as stopped.
	stillFor = 1200 * time.Millisecond
	tick     = 200 * time.Millisecond
)

// beatFile names a new heartbeat, "" when the run cannot reach the suite's files.
func (r *run) beatFile() string {
	if r.opt.Remote {
		return ""
	}
	r.seq++
	p := filepath.Join(r.dir, "beat-"+strconv.Itoa(r.seq))
	r.beats = append(r.beats, p)
	return p
}

// longScript is the probe arguments of a run that goes on until it is ended.
func longScript(beat string, ttl int) []string {
	if beat == "" {
		return []string{"sleep", strconv.Itoa(ttl)}
	}
	return []string{"beat", beat}
}

func size(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

// lastBeat is when the heartbeat last grew.
func lastBeat(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// sleep waits d, or until the run is cancelled.
func (r *run) sleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-r.ctx.Done():
	}
}

// waitBeat waits for the heartbeat to appear.
func (r *run) waitBeat(beat string) error {
	deadline := time.Now().Add(r.opt.RunWait)
	for size(beat) == 0 {
		if !time.Now().Before(deadline) || r.ctx.Err() != nil {
			return fmt.Errorf("no heartbeat within %s (a run that cannot reach %s needs --remote)", r.opt.RunWait, r.dir)
		}
		r.sleep(tick)
	}
	return nil
}

// beating waits for the heartbeat file to grow.
func (r *run) beating(beat string, within time.Duration) error {
	from, deadline := size(beat), time.Now().Add(within)
	for size(beat) == from {
		if !time.Now().Before(deadline) || r.ctx.Err() != nil {
			return fmt.Errorf("no heartbeat for %s: the run is not going", within)
		}
		r.sleep(tick)
	}
	return nil
}

// waitStopped waits for every file to stop growing, by deadline.
func (r *run) waitStopped(deadline time.Time, files ...string) error {
	var all []string
	for _, f := range files {
		all = append(all, f, f+".child")
	}
	sizes := map[string]int64{}
	still, last := time.Now(), "" // last: the file seen growing most recently (a loaded machine may skip a tick)
	for _, f := range all {
		sizes[f] = size(f)
	}
	for {
		grew := ""
		for _, f := range all {
			if n := size(f); n != sizes[f] {
				sizes[f], grew = n, f
			}
		}
		if grew != "" {
			still, last = time.Now(), grew
		}
		switch {
		case time.Since(still) >= stillFor:
			return nil
		case !time.Now().Before(deadline) || r.ctx.Err() != nil:
			what := "the run is still going"
			if strings.HasSuffix(last, ".child") {
				what = "a child of the run is still going: the process group was not killed"
			}
			return fmt.Errorf("%s past the deadline", what)
		}
		r.sleep(tick)
	}
}

// runners counts the different processes that wrote a heartbeat.
func runners(beat string) int {
	b, err := os.ReadFile(beat)
	if err != nil {
		return 0
	}
	seen := map[string]bool{}
	for _, l := range strings.Fields(string(b)) {
		seen[l] = true
	}
	return len(seen)
}

// dirty leaves uncommitted changes in the worktree: a change to a tracked file
// and an untracked file. The returned func puts them back, but only where the
// file still holds what dirty wrote: someone else's edit since is kept, and
// the error says so.
func dirty(worktree string) (func() error, error) {
	out, err := exec.Command("git", "-C", worktree, "ls-files").Output()
	if err != nil {
		return nil, fmt.Errorf("--worktree %s: git ls-files: %v", worktree, err)
	}
	files := strings.Fields(string(out))
	if len(files) == 0 {
		return nil, fmt.Errorf("--worktree %s tracks no file", worktree)
	}
	tracked := filepath.Join(worktree, files[0])
	st, err := os.Stat(tracked)
	if err != nil {
		return nil, err
	}
	before, err := os.ReadFile(tracked)
	if err != nil {
		return nil, err
	}
	untracked := filepath.Join(worktree, "zm-run-conformance-dirty")
	if _, err := os.Lstat(untracked); err == nil {
		return nil, fmt.Errorf("%s exists: a run that was cut off? Check it and remove it", untracked)
	}
	wrote := append(bytes.Clone(before), []byte("\n# zm-run-conformance dirty\n")...)
	const extra = "uncommitted\n"
	if err := os.WriteFile(tracked, wrote, st.Mode()); err != nil {
		return nil, err
	}
	undo := func() error {
		var kept []string
		if now, err := os.ReadFile(tracked); err == nil && bytes.Equal(now, wrote) {
			_ = os.WriteFile(tracked, before, st.Mode())
		} else {
			kept = append(kept, tracked)
		}
		if now, err := os.ReadFile(untracked); err == nil && string(now) == extra {
			_ = os.Remove(untracked)
		} else if err == nil {
			kept = append(kept, untracked)
		}
		if len(kept) > 0 {
			return fmt.Errorf("changed since the suite dirtied them, so left as they are: %s", strings.Join(kept, ", "))
		}
		return nil
	}
	if err := os.WriteFile(untracked, []byte(extra), 0o644); err != nil {
		_ = undo()
		return nil, err
	}
	return undo, nil
}

// worktreeHead is the worktree's HEAD, "" when it has none.
func worktreeHead(worktree string) string {
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
