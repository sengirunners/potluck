#!/bin/sh
# nc: run one step on this run's clean machine, timed and recorded.
#
#   nc <step-name> '<command line>'          run it now; exits with the command's own code
#   nc --bg <step-name> '<command line>'     start it in the background, for a command that may run longer than
#                                            you can wait in one go; returns at once
#   nc --wait <step-name> [seconds]          wait for a --bg step, at most that long (default 540): exits with the
#                                            step's own code when it ends, 75 while it is still running
#
# A step name is letters, digits, dot, dash and underscore. There is no keyboard: a prompt gets no answer.
#
# For the harness: STRANGER_RUN names the run directory that run.sh made. Its file `machine` says where the clean
# machine is: "mac" (a fresh HOME, TART_HOME and PATH on this Mac) or "linux <vm>" (a fresh Tart Ubuntu VM, reached
# with `tart exec`). Every step appends a row to timings.tsv (step, start, end, seconds, exit, output file under
# steps/, command), its output to terminal.log and that file, and its terminal to terminal.rec (BSD `script -r`: play it back with
# `script -p terminal.rec`). A step that outlives STRANGER_STEP_TIMEOUT is stopped by `timeout`, which signals
# `script`; a child that started its own session (a daemon) can outlive it, and run.sh --cleanup stops those.
set -u

case ${1:-} in "" | -h | --help) sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;; esac

# Runs read these scripts while they change: edit them by writing a new file and renaming it over the old one.
RUN=${STRANGER_RUN:?STRANGER_RUN names the run directory}
STEP_TIMEOUT=${STRANGER_STEP_TIMEOUT:-7200}
STILL_RUNNING=75

die() {
	printf 'nc: %s\n' "$*" >&2
	exit 2
}

check_name() {
	case $1 in
	"" | *[!A-Za-z0-9._-]* | .*) die "step name '$1': use letters, digits, dot, dash and underscore, not starting with a dot" ;;
	esac
}

# on_machine LINE: run LINE on the clean machine (in place of this process).
on_machine() {
	read -r kind vm <"$RUN/machine"
	case $kind in
	mac)
		exec env -i HOME="$RUN/home" USER="$(id -un)" LOGNAME="$(id -un)" SHELL=/bin/zsh TERM=xterm-256color \
			LANG=en_US.UTF-8 TMPDIR="$RUN/home/tmp/" TART_HOME="$RUN/home/.tart" \
			PATH="$RUN/harness-bin:$RUN/home/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
			/bin/zsh -c '[ -f "$HOME/.zshrc" ] && . "$HOME/.zshrc"; cd "$HOME"; eval "$1"' zsh "$1"
		;;
	linux)
		exec env TART_HOME="$RUN/tart" "$(cat "$RUN/tart-cli")" exec "$vm" bash -lc 'cd "$HOME"; eval "$1"' bash "$1"
		;;
	*) die "unknown machine kind in $RUN/machine: $kind" ;;
	esac
}

# `script` echoes the end of its empty input as ^D and two backspaces; perl drops them from the output.
run_step() {
	name=$1 line=$2
	mkdir -p "$RUN/steps"
	start=$(date +%s)
	# Named by start time and this process, not by row: a background step that ends meanwhile adds its row first.
	outname=$start-$$-$name.out
	out=$RUN/steps/$outname
	printf '\n$ %s    # step %s, %s\n' "$line" "$name" "$(date -u +%H:%M:%SZ)" | tee -a "$RUN/terminal.log"
	{
		timeout "$STEP_TIMEOUT" script -q -r -a "$RUN/terminal.rec" "$0" --on-machine "$line" </dev/null
		echo $? >"$RUN/.rc.$$"
	} 2>&1 | perl -pe 'BEGIN { $| = 1 } s/\^D\x08\x08//g' | tee "$out" | tee -a "$RUN/terminal.log"
	rc=$(cat "$RUN/.rc.$$")
	rm -f "$RUN/.rc.$$"
	end=$(date +%s)
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$name" "$start" "$end" "$((end - start))" "$rc" "$outname" \
		"$(printf '%s' "$line" | tr '\t\n\r' '   ')" >>"$RUN/timings.tsv"
	printf '# step %s: exit %s after %ss\n' "$name" "$rc" "$((end - start))" | tee -a "$RUN/terminal.log"
	case $rc in [0-9]*) return "$rc" ;; *) return 1 ;; esac
}

case ${1:-} in
--bg)
	[ $# -eq 3 ] || die "usage: nc --bg <step-name> '<command line>'"
	check_name "$2"
	mkdir -p "$RUN/bg"
	[ ! -e "$RUN/bg/$2.pid" ] || die "step $2 already started"
	(STRANGER_RUN=$RUN "$0" "$2" "$3" >"$RUN/bg/$2.out" 2>&1; echo $? >"$RUN/bg/$2.done") &
	echo $! >"$RUN/bg/$2.pid"
	echo "started step $2 in the background; wait for it with: nc --wait $2"
	;;
--wait)
	[ $# -ge 2 ] || die "usage: nc --wait <step-name> [seconds]"
	check_name "$2"
	[ -e "$RUN/bg/$2.pid" ] || die "no background step $2"
	limit=${3:-540}
	waited=0
	while :; do
		# .done first: a step that ended (even by its timeout, exit 124) reports its own code, never "running".
		if [ -e "$RUN/bg/$2.done" ]; then
			tail -n 40 "$RUN/bg/$2.out"
			exit "$(cat "$RUN/bg/$2.done")"
		fi
		if ! kill -0 "$(cat "$RUN/bg/$2.pid")" 2>/dev/null; then
			sleep 1
			[ -e "$RUN/bg/$2.done" ] && continue
			tail -n 5 "$RUN/bg/$2.out"
			die "background step $2 ended without an exit code (killed?)"
		fi
		if [ "$waited" -ge "$limit" ]; then
			echo "step $2 still running after ${waited}s more; last lines:"
			tail -n 5 "$RUN/bg/$2.out"
			exit "$STILL_RUNNING"
		fi
		sleep 10
		waited=$((waited + 10))
	done
	;;
--on-machine) on_machine "$2" ;;
*)
	[ $# -eq 2 ] || die "usage: nc <step-name> '<command line>'"
	check_name "$1"
	run_step "$1" "$2"
	;;
esac
