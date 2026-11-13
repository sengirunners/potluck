#!/bin/sh
# clean-machine-proof.sh: the README quick start on a fresh Linux machine with
# no zm. It installs Potluck from its newest signed release, starts a pool,
# shares this machine with it from a second home over a Cloudflare quick tunnel
# (an https invite code), and runs a sandbox and a job on it. Two daemons on one
# machine: "owner" runs the pool, "lender" shares the machine. It needs sudo
# (the root step potluck doctor prints), docker, curl, git and jq, and GH_TOKEN
# while the repository is private. It changes the machine (gVisor, a systemd
# unit, a docker network, cloudflared): run it on a throwaway VM only, as
# .github/workflows/clean-machine-proof.yml does.
set -eu

WORK=${WORK:-$(mktemp -d)}
BIN=$HOME/.local/bin
OWNER=$WORK/owner
LENDER=$WORK/lender
PORT=7435
export PATH="$BIN:$PATH"

step() { printf '\n== %s  %s\n' "$(date -u +%H:%M:%SZ)" "$*"; }
die() { printf 'PROOF FAILED: %s\n' "$*" >&2; exit 1; }
po() { POTLUCK_HOME=$OWNER POTLUCK_MACHINE=owner potluck "$@"; }
pl() { POTLUCK_HOME=$LENDER POTLUCK_MACHINE=lender potluck "$@"; }

step "machine"
uname -a
. /etc/os-release && echo "$PRETTY_NAME"
nproc
free -m | sed -n 2p
openssl version

step "no zm here"
if command -v zm >/dev/null 2>&1; then die "zm is on PATH: $(command -v zm)"; fi
for p in "$HOME/.zm" "$HOME/.local/bin/zm" /usr/local/bin/zm; do
	[ ! -e "$p" ] || die "$p exists"
done
echo "no zm binary, no ~/.zm"

step "install from the signed release (README quick start)"
printf 'Authorization: Bearer %s\n' "$GH_TOKEN" |
	curl -fsSL -H @- https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck --version
ls -l "$BIN"
if command -v zm >/dev/null 2>&1; then die "the install put zm on PATH"; fi

step "tunnel: cloudflared quick tunnel to the owner's loopback listener"
curl -fsSL -o "$WORK/cloudflared.deb" https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64.deb
sudo dpkg -i "$WORK/cloudflared.deb" >/dev/null
cloudflared --version
cloudflared tunnel --no-autoupdate --url "http://127.0.0.1:$PORT" >"$WORK/cloudflared.log" 2>&1 &
URL=
for _ in $(seq 60); do
	URL=$(grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' "$WORK/cloudflared.log" | head -1 || true)
	[ -z "$URL" ] || break
	sleep 1
done
[ -n "$URL" ] || { cat "$WORK/cloudflared.log"; die "no quick tunnel URL"; }
echo "tunnel $URL"

step "owner: a home whose tunnel carries the pool"
mkdir -m 700 -p "$OWNER" "$LENDER"
umask 077
printf '{"tunnel":{"listen":"127.0.0.1:%s","url":"%s"}}\n' "$PORT" "$URL" >"$OWNER/config.json"
po daemon --detach
po status || true
# the quick tunnel's hostname takes a few seconds to resolve everywhere
for _ in $(seq 60); do
	if curl -s -o /dev/null -w '%{http_code}\n' "$URL/" | grep -qv '^000$\|^530$\|^502$'; then break; fi
	sleep 2
done
curl -s -o /dev/null -w 'tunnel answers: HTTP %{http_code}\n' "$URL/" || true

step "owner: create the pool"
po create proof --via tunnel
CODE=$(po invite lender --host --via tunnel --json | jq -r .code)
case $CODE in https://*) echo "host invite: https code for $URL" ;; *) die "host invite is not a tunnel code: $CODE" ;; esac

step "lender: a second home on this machine"
pl daemon --detach
pl doctor || true
ROOT=$(pl doctor 2>/dev/null | sed -n 's/^ *\(sudo sh -c .*\)$/\1/p' | head -1)
if [ -n "$ROOT" ]; then
	step "lender: the root step doctor named"
	echo "$ROOT"
	eval "$ROOT"
	pl doctor || true
fi

step "lender: share this machine with the pool (potluck host '<code>')"
pl host "$CODE" --layer oci --sizes c1m2 --share 1 --no-sudo

step "owner: the pool lists the shared machine"
po status
po sandbox ls || true

step "owner: run a job in a sandbox on the lender (potluck sandbox create/exec)"
OUT=$(po sandbox create --size c1m2 --ttl 15m)
echo "$OUT"
ID=${OUT%% *}
case $ID in lender-c1m2@lender/*) ;; *) die "sandbox not placed on the lender: $OUT" ;; esac
po sandbox exec "$ID" -- sh -c 'uname -a; id; echo hi > note; cat note; command -v zm || echo "no zm in the box"'
[ "$(po sandbox exec "$ID" -- cat note)" = hi ] || die "the sandbox kept no state"
PID=$(po sandbox exec "$ID" --detach -- sh -c 'echo one; sleep 1; echo two; exit 5')
set +e
po sandbox logs "$ID" "$PID" -f
rc=$?
set -e
[ "$rc" = 5 ] || die "logs -f exit $rc, want 5"
po sandbox rm "$ID"

step "owner: potluck run (a commit's job in a fresh sandbox)"
J=$WORK/job
mkdir -p "$J/.potluck" && cd "$J"
git init -q && git config user.email proof@example.com && git config user.name proof
printf '#!/bin/sh\nset -e\necho "job at $(git rev-parse --short HEAD) on $(hostname)"\ntest -f README\necho job-ok\n' >.potluck/job
chmod +x .potluck/job
echo proof >README
git add -A && git commit -qm proof
po run --size c1m2 --timeout 10m | tee "$WORK/job.out"
grep -q job-ok "$WORK/job.out" || die "the job did not print job-ok"
cd "$WORK"

step "lender: stop sharing and leave; daemons stop"
pl host stop
pl leave
po usage || true
pl daemon stop
po daemon stop

step "PROOF OK: installed with no zm, shared this machine with a pool, ran a sandbox and a job on it"
