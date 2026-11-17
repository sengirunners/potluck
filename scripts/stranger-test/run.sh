#!/bin/sh
# run.sh: one stranger-test run. A newcomer with only the public instructions tries to reach a sandbox that
# answers `uname -a` (README "Quick start") on a machine with nothing of ours on it. See README.md here.
#
#   run.sh --platform mac|linux --mode agent|scripted [--out DIR] [--model sonnet]
#   run.sh --cleanup DIR        stop and delete what a run made (its VM, its daemons, its TART_HOME)
#   run.sh --report DIR         rebuild report.md; exits 1 when the run did not reach the goal
#
# Exit 0 only when the run reached the goal (see verdict below): a step that ran
# `potluck sandbox exec <id> -- uname -a` on the clean machine exited 0 and printed something.
#
# It runs on an Apple-silicon Mac with Tart. The clean machine is:
#   mac    a fresh HOME, TART_HOME and PATH (/usr/bin:/bin:/usr/sbin:/sbin plus ~/.local/bin) on this Mac, so
#          nothing in /usr/local, /opt/homebrew or the real home is found and the first image pull is real.
#   linux  a fresh Ubuntu VM (ghcr.io/cirruslabs/ubuntu:latest) cloned into the run's own TART_HOME.
# The run directory gets: machine-before.txt (what this host already had that a newcomer would not),
# timings.tsv, terminal.log, terminal.rec (`script -p terminal.rec` plays it back), stumbles.md, report.md.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd -P)
UBUNTU=${STRANGER_UBUNTU_IMAGE:-ghcr.io/cirruslabs/ubuntu:latest}
# Where run.sh keeps the stock Ubuntu image between runs, so each Linux run clones it instead of pulling 3 GB.
# The image is a stock Ubuntu with nothing of ours: the newcomer's Linux box, not part of what they install.
BASE_TART=${STRANGER_BASE_TART_HOME:-$HOME/.cache/potluck-stranger/linux-base-tart}

die() {
	printf 'run.sh: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	run=$1
	[ -f "$run/machine" ] || die "$run is not a stranger-test run directory"
	read -r kind vm <"$run/machine"
	case $kind in
	mac)
		# A daemon the newcomer started writes its pid to daemon.lock in its home. Stop it only when that pid is
		# still a program from the run's own home (a pid can be reused by an unrelated process).
		for lock in "$run"/home/.potluck/daemon.lock; do
			[ -f "$lock" ] || continue
			pid=$(tr -cd '0-9' <"$lock")
			[ -n "$pid" ] || continue
			cmd=$(ps -o command= -p "$pid" 2>/dev/null || true)
			case $cmd in
			"$run/home/"*) kill "$pid" && echo "stopped daemon pid $pid ($cmd)" ;;
			"") ;;
			*) echo "left pid $pid alone: it is no longer the run's daemon ($cmd)" ;;
			esac
		done
		if [ -d "$run/home/.tart/vms" ]; then
			for v in "$run"/home/.tart/vms/*; do
				[ -d "$v" ] || continue
				TART_HOME=$run/home/.tart timeout 60 tart stop "$(basename "$v")" 2>/dev/null || true
				TART_HOME=$run/home/.tart timeout 60 tart delete "$(basename "$v")" || true
			done
		fi
		rm -rf "$run/home/.tart"
		;;
	linux)
		TART_HOME=$run/tart timeout 60 tart stop "$vm" 2>/dev/null || true
		TART_HOME=$run/tart timeout 60 tart delete "$vm" || true
		rm -rf "$run/tart"
		;;
	esac
	echo "cleaned $run (kept its logs)"
}

inventory_mac() {
	echo "== host: $(sw_vers -productName) $(sw_vers -productVersion), $(sysctl -n machdep.cpu.brand_string)"
	echo "== disk free on /: $(df -h / | awk 'NR==2 {print $4}')"
	echo "== present on this host that a newcomer would not have (hidden from the run by its PATH and HOME):"
	for c in tart softnet potluck zm docker brew go claude; do
		p=$(command -v "$c" 2>/dev/null || true)
		[ -n "$p" ] && echo "  $c: $p"
	done
	[ -u /usr/local/bin/softnet ] && echo "  /usr/local/bin/softnet is already setuid root (the sudo step is done on this host)"
	[ -d "$HOME/.tart" ] && echo "  ~/.tart holds $(du -sh "$HOME/.tart" 2>/dev/null | cut -f1) of images (the run uses its own TART_HOME)"
	[ -d "$HOME/.potluck" ] && echo "  ~/.potluck exists (the run uses its own HOME)"
	[ -d "$HOME/.local/state/potluck/daemons" ] && echo "  daemon records: $(ls "$HOME/.local/state/potluck/daemons" | tr '\n' ' ')"
	echo "== left visible: /usr/bin, /bin, /usr/sbin, /sbin (stock macOS), and this Mac's network, Keychain and login"
}

# preflight RUN MIN_FREE_GB MACOS: refuse a run the host cannot take. Apple lets a Mac run at most two macOS VMs at
# once; Virtualization.framework runs one com.apple.Virtualization.VirtualMachine process per VM of any OS, so this
# counts Linux VMs too and refuses early rather than late.
preflight() {
	run=$1 floor=$2 macos=$3
	free=$(df -g "$run" | awk 'NR==2 {print $4}')
	[ "$free" -ge "$floor" ] || die "only $free GB free where the run lives; this run wants $floor GB (STRANGER_MIN_FREE_GB)"
	if [ "$macos" = yes ]; then
		vms=$(ps -axo comm= | awk -F/ '$NF == "com.apple.Virtualization.VirtualMachine"' | wc -l | tr -d ' ')
		[ "$vms" -lt 2 ] || die "$vms VMs already run on this Mac, and macOS allows two macOS VMs at once: wait for one to stop"
	fi
}

setup_mac() {
	run=$1
	# The first potluck host pulls a macOS image of about 30 GB, and doctor wants 60 GB free.
	preflight "$run" "${STRANGER_MIN_FREE_GB:-70}" yes
	mkdir -p "$run/home/tmp" "$run/harness-bin"
	# The clean Mac home is still this Mac, so a run never gets root on it: this sudo, first on the run's PATH,
	# refuses at once (sudo on the recording's terminal would otherwise wait forever for a password, or, on a host
	# with a sudo ticket or NOPASSWD, give the run real root). STRANGER_ALLOW_SUDO=1 passes it to `sudo -n`.
	if [ "${STRANGER_ALLOW_SUDO:-}" = 1 ]; then
		printf '#!/bin/sh\nexec /usr/bin/sudo -n "$@"\n' >"$run/harness-bin/sudo"
	else
		cat >"$run/harness-bin/sudo" <<'SHIM'
#!/bin/sh
echo "stranger-test: sudo is refused in this run: the clean home is on the test host, and a run never gets root on it (a newcomer would type their password here)" >&2
exit 1
SHIM
	fi
	chmod +x "$run/harness-bin/sudo"
	inventory_mac >"$run/machine-before.txt"
	echo mac >"$run/machine"
}

setup_linux() {
	run=$1
	command -v tart >/dev/null || die "the Linux run needs Tart on this Mac"
	# The VM's disk is sparse; the run fills about 15 GB of it (Docker and Potluck's box images).
	preflight "$run" "${STRANGER_MIN_FREE_GB:-25}" no
	if ! TART_HOME=$BASE_TART tart list --format json 2>/dev/null | grep -q "\"$UBUNTU\""; then
		echo "pulling $UBUNTU once into $BASE_TART (about 3 GB)"
		TART_HOME=$BASE_TART timeout 3600 tart pull "$UBUNTU"
	fi
	vm=stranger-$(date -u +%H%M%S)
	mkdir -p "$run/tart"
	# A clone from the cache, so the run's TART_HOME is its own and deleting it touches nothing else.
	cp -c -R "$BASE_TART/." "$run/tart/" 2>/dev/null || cp -R "$BASE_TART/." "$run/tart/"
	TART_HOME=$run/tart timeout 600 tart clone "$UBUNTU" "$vm"
	# 8 GB by default: at Cirrus's 4 GB no sandbox size fits in the half Potluck shares (run linux-scripted-2).
	TART_HOME=$run/tart timeout 60 tart set "$vm" --disk-size 60 --cpu "${STRANGER_LINUX_CPUS:-4}" --memory "${STRANGER_LINUX_MEMORY_MB:-8192}"
	echo "linux $vm" >"$run/machine"
	(TART_HOME=$run/tart tart run --no-graphics "$vm" >"$run/vm.log" 2>&1 &)
	for _ in $(seq 60); do
		TART_HOME=$run/tart timeout 10 tart exec "$vm" true 2>/dev/null && break
		sleep 5
	done
	TART_HOME=$run/tart timeout 10 tart exec "$vm" true || die "the VM did not answer tart exec in 5 minutes ($run/vm.log)"
	TART_HOME=$run/tart tart exec "$vm" sh -c 'sudo growpart /dev/vda 1 >/dev/null 2>&1; sudo resize2fs /dev/vda1 >/dev/null 2>&1; true'
	{
		echo "== host: a fresh Tart VM from $UBUNTU on $(sw_vers -productName) $(sw_vers -productVersion)"
		TART_HOME=$run/tart tart exec "$vm" sh -c '. /etc/os-release; echo "== guest: $PRETTY_NAME, $(uname -m), $(nproc) cpus, $(free -m | awk "NR==2{print \$2}") MB RAM, $(df -h / | awk "NR==2{print \$4}") free"
			for c in docker potluck zm runsc kata-runtime go; do p=$(command -v $c); [ -n "$p" ] && echo "  already has $c: $p"; done; true'
		echo "== the guest is Cirrus Labs' stock image: user admin with passwordless sudo, Tart's guest agent"
	} >"$run/machine-before.txt"
}

agent_prompt() {
	run=$1 platform=$2
	sed -e "s|@NC@|$run/nc|g" -e "s|@STUMBLES@|$run/stumbles.md|g" -e "s|@PLATFORM@|$platform|g" "$HERE/newcomer.md"
}

# verdict RUN: "PASS" or "FAIL". The harness decides from nc's own record, not from the agent's reply. PASS needs
# a step whose whole command is `potluck sandbox exec <id> -- uname -a`, that exited 0 and whose first line starts
# with "Linux " or "Darwin ", where <id> was listed as ready by a `potluck sandbox create` step that ended before
# it started. Column 6 of timings.tsv names each step's output file, column 7 is its command.
verdict() {
	run=$1
	t=$run/timings.tsv
	[ -s "$t" ] || { echo FAIL; return; }
	for n in $(awk -F'\t' '$5 == "0" && $7 ~ /^potluck sandbox exec [^ ;&|`$()<>]+ -- uname -a$/ { print NR }' "$t"); do
		set -- $(awk -F'\t' -v n="$n" 'NR == n { split($7, w, " "); print w[4], $2, $6 }' "$t")
		id=$1 start=$2 out=$3
		tr -d '\r' <"$run/steps/$out" 2>/dev/null | grep -v '^[[:space:]]*$' | head -n 1 | grep -Eq '^(Linux|Darwin) ' || continue
		for c in $(awk -F'\t' -v s="$start" '$5 == "0" && $3 <= s && $7 ~ /^potluck sandbox create( |$)/ { print $6 }' "$t"); do
			if tr -d '\r' <"$run/steps/$c" 2>/dev/null | awk -v id="$id" '$1 == id && / ready( |$)/ { f = 1 } END { exit !f }'; then
				echo PASS
				return
			fi
		done
	done
	echo FAIL
}

report() {
	run=$1
	v=$(verdict "$run")
	read -r kind _ <"$run/machine"
	{
		echo "# Stranger test: $(cat "$run/machine") ($(cat "$run/mode"))"
		echo
		if [ "$v" = PASS ]; then
			echo "**Verdict: PASS**: a sandbox on the clean machine answered \`uname -a\`."
		else
			echo "**Verdict: FAIL**: no \`potluck sandbox exec <id> -- uname -a\` step exited 0 with output. Last step reached: $(tail -n 1 "$run/timings.tsv" | awk -F'\t' '{ printf "%s (exit %s)", $1, $5 }')."
		fi
		echo
		if [ -s "$run/agent.jsonl" ]; then
			echo "Agent model: $(sed -n 's/.*"model":"\([^"]*\)".*/\1/p' "$run/agent.jsonl" | head -n 1)"
			echo
		fi
		cat "$run/machine-before.txt"
		if tr -d '\r' <"$run/terminal.log" | grep -qi '^location: .*/releases/tag/'; then
			echo "== downloaded during the run:"
			tr -d '\r' <"$run/terminal.log" | grep -i '^location: .*/releases/tag/' | sed 's|.*github.com/\([^/]*/[^/]*\)/releases/tag/\(.*\)|  \1 \2|'
		fi
		[ "$kind" = linux ] && echo "== note: the stock image's sudo needs no password, so the root step ran as printed; on a real box it asks for one"
		echo
		echo "## Substituted steps"
		echo
		if tr -d '\r' <"$run/terminal.log" | grep -q '^SUBSTITUTED:'; then
			tr -d '\r' <"$run/terminal.log" | grep '^SUBSTITUTED:' | sed 's/^/- /'
		else
			echo "(no harness-marked substitutions)"
		fi
		echo
		echo "## Steps"
		echo
		echo "| step | seconds | exit |"
		echo "| --- | ---: | ---: |"
		awk -F'\t' '{ printf "| %s | %s | %s |\n", $1, $4, $5; t += $4 } END { printf "| **command time** | %d | |\n", t }' "$run/timings.tsv" 2>/dev/null
		echo
		echo "Command time is the sum of the steps; it leaves out the agent's own reading and thinking."
		echo
		echo "## Stumbles"
		echo
		if [ -s "$run/stumbles.md" ]; then cat "$run/stumbles.md"; else echo "(none written)"; fi
	} >"$run/report.md"
	echo "report: $run/report.md ($v)"
	[ "$v" = PASS ]
}

# main is one function, so sh has read all of it before it runs: editing this file mid-run cannot break a run.
main() {
	PLATFORM= MODE= OUT= MODEL=sonnet
	while [ $# -gt 0 ]; do
		case $1 in
		--platform) PLATFORM=$2; shift 2 ;;
		--mode) MODE=$2; shift 2 ;;
		--out) OUT=$2; shift 2 ;;
		--model) MODEL=$2; shift 2 ;;
		--cleanup) cleanup "$2"; exit 0 ;;
		--report) report "$2"; exit $? ;;
		*) die "unknown argument $1 (see the top of $0)" ;;
		esac
	done
	case $PLATFORM in mac | linux) ;; *) die "--platform mac|linux" ;; esac
	case $MODE in agent | scripted) ;; *) die "--mode agent|scripted" ;; esac
	[ "$(uname -s)/$(uname -m)" = Darwin/arm64 ] || die "runs on an Apple-silicon Mac"

	RUN=${OUT:-${STRANGER_RUNS:-$HOME/.cache/potluck-stranger/runs}/$PLATFORM-$MODE-$(date -u +%Y%m%dT%H%M%SZ)}
	if [ -d "$RUN" ] && [ -n "$(ls -A "$RUN")" ]; then die "$RUN is not empty: give --out a new directory"; fi
	mkdir -p "$RUN"
	RUN=$(cd "$RUN" && pwd -P)
	echo "$MODE" >"$RUN/mode"
	: >"$RUN/timings.tsv"
	printf '#!/bin/sh\nSTRANGER_RUN=%s exec %s "$@"\n' "$RUN" "$HERE/nc.sh" >"$RUN/nc"
	chmod +x "$RUN/nc"
	echo "run directory: $RUN"

	"setup_$PLATFORM" "$RUN"
	cat "$RUN/machine-before.txt"

	case $MODE in
	scripted)
		STRANGER_RUN=$RUN "$HERE/scripted-$PLATFORM.sh" || echo "scripted run stopped at a failed step (see timings.tsv)"
		;;
	agent)
		# A fresh session with no CLAUDE.md, memory or repo: an empty working directory and project-only settings.
		# --strict-mcp-config with no --mcp-config: none of this user's MCP servers. --tools: only these built-in
		# tools exist; --allowedTools: only nc, the public web and the stumbles file run without a prompt, and -p
		# refuses the rest. This is not a sandbox: nc runs as this user, so on the Mac run the clean shell can read
		# anything the user can (README.md here says so).
		# File rules take an absolute path as //path: "/$RUN" is that, since $RUN starts with a slash.
		mkdir -p "$RUN/agent-cwd"
		agent_prompt "$RUN" "$PLATFORM" >"$RUN/agent-prompt.md"
		(cd "$RUN/agent-cwd" && timeout 10800 claude -p --setting-sources project --strict-mcp-config --model "$MODEL" \
			--tools Bash,WebFetch,WebSearch,Read,Write,Edit \
			--allowedTools "Bash($RUN/nc:*)" WebFetch WebSearch "Write(/$RUN/stumbles.md)" "Edit(/$RUN/stumbles.md)" "Read(/$RUN/stumbles.md)" \
			--output-format stream-json --verbose <"$RUN/agent-prompt.md" >"$RUN/agent.jsonl" 2>"$RUN/agent.err") ||
			echo "the agent session ended with an error (see agent.err)"
		;;
	esac

	echo "when you are done with it: $0 --cleanup $RUN"
	report "$RUN"
}

main "$@"
