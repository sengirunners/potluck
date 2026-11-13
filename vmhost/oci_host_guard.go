package vmhost

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Confirmed controller expiry is converted once to the host's monotonic uptime.
// A queued/stale message cannot buy a new timeout when the guard receives it.
func hostDeadline(exp time.Time, reserve time.Duration) (int64, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) < 1 {
		return 0, fmt.Errorf("no monotonic host uptime")
	}
	u, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	left := time.Until(exp) - reserve
	if left <= 0 || left > time.Hour {
		return 0, fmt.Errorf("confirmed admission expiry leaves no bounded cleanup window")
	}
	return int64(u*100) + left.Milliseconds()/10, nil
}

type hostGuardPipe struct {
	io.WriteCloser
	mu      sync.Mutex
	reserve time.Duration
}

func (p *hostGuardPipe) confirm(exp time.Time) error {
	d, err := hostDeadline(exp, p.reserve)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err = fmt.Fprintf(p.WriteCloser, "%d\n", d)
	return err
}

// The existing OCI deadman is also the deadline reader. There is no separate
// cleanup child for an attach-exit path to cancel. Every native/Docker command
// is bounded, and native kill does not wait behind a stuck Docker CLI.
const ociHostDeadman = `vm=$1; bin=$2; parent=$3; cg=$4; deadline=$5; root=$6
[[ $parent =~ ^potluck-[0-9a-f]{64}\.slice$ && $cg == "$root/potluck.slice/$parent" && $deadline =~ ^[0-9]{1,12}$ ]] || exit 125
p=; cleaned=0
empty_parent() {
  [[ ! -e $cg ]] && return 0
  local key value
  while read -r key value; do [[ $key == populated && $value == 0 ]] && return 0; done < "$cg/cgroup.events"
  return 1
}
cleanup() {
  (( cleaned )) && return 0
  cleaned=1; trap - EXIT CHLD TERM INT HUP
  timeout -s KILL 2 sudo -n sh -c 'test ! -e "$1" || printf 1 > "$1/cgroup.kill"' sh "$cg" >/dev/null 2>&1
  timeout -s KILL 2 sudo -n busctl call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss "$parent" replace >/dev/null 2>&1
  timeout -s KILL 2 "$bin" rm -f "$vm" >/dev/null 2>&1
  [[ -z $p ]] || kill -KILL "$p" 2>/dev/null
  for ((i=0;i<20;i++)); do empty_parent && return 0; sleep .05; done
  echo 'owned host resource parent did not empty' >&2; return 1
}
now_cs() {
  local up unused whole fraction
  read -r up unused < /proc/uptime || return 1
  whole=${up%%.*}; fraction=${up#*.}00; fraction=${fraction:0:2}
  now=$((10#$whole*100+10#$fraction))
}
trap 'cleanup' EXIT
trap 'exit 125' TERM INT HUP
now_cs || exit 125
(( deadline > now && deadline-now <= 360000 )) || exit 125
child_done() {
  [[ -z $p ]] && return
  kill -0 "$p" 2>/dev/null && return
  wait "$p"; local status=$?
  cleanup || status=125
  exit "$status"
}
"$bin" start -a "$vm" </dev/null & p=$!
while :; do
  child_done
  now_cs || exit 125
  left=$((deadline-now)); (( left > 0 )) || exit 125
  # A short read timeout also observes attach exit without a reentrant CHLD
  # trap reaping its status or interrupting cleanup.
  (( left > 25 )) && left=25
  printf -v wait_s '%d.%02d' "$((left/100))" "$((left%100))"
  if IFS= read -r -t "$wait_s" next; then
    [[ $next =~ ^[0-9]{1,12}$ ]] || exit 125
    now_cs || exit 125
    (( next > now && next-now <= 360000 )) || exit 125
    (( next >= deadline )) || exit 125
    deadline=$next
  else
    read_status=$?
    (( read_status > 128 )) && continue
    exit 125
  fi
done`
