#!/bin/sh
# scripted-mac.sh: the README quick start on the clean Mac, step by step, as written. No agent: nothing here can
# guess, so a step the README does not spell out is done the way the tool's own site says, and named in the run's
# stumbles by whoever reads the log. Called by run.sh with STRANGER_RUN set.
set -u
nc() { "$STRANGER_RUN/nc" "$@"; }

# One function, read whole before it runs (see run.sh).
main() {
	nc install 'curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh' || exit 1
	nc path "echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.zshrc && . ~/.zshrc && command -v potluck"
	nc doctor-1 'potluck doctor'
	# README "What you need" links tart.run's quick start, which installs Tart with Homebrew. The clean Mac has no
	# Homebrew, so this takes the release tarballs doctor names, and logs which release "latest" was.
	nc tart 'curl -sI https://github.com/openai/tart/releases/latest | grep -i "^location:"; mkdir -p ~/Applications ~/.local/bin && curl -fsSL https://github.com/openai/tart/releases/latest/download/tart.tar.gz | tar -xz -C ~/Applications && ln -sf ~/Applications/tart.app/Contents/MacOS/tart ~/.local/bin/tart && tart --version'
	nc softnet 'curl -sI https://github.com/openai/softnet/releases/latest | grep -i "^location:"; curl -fsSL https://github.com/openai/softnet/releases/latest/download/softnet.tar.gz | tar -xz -C ~/.local/bin softnet && ls -l ~/.local/bin/softnet && softnet --version'
	nc doctor-2 'potluck doctor'
	# Doctor's root step, as printed. The run's sudo refuses (run.sh: no password can be typed, and the harness gives
	# a run no root on this Mac unless STRANGER_ALLOW_SUDO=1). When it is refused and the step is the Softnet one,
	# the run stands in this host's own Softnet, only if that is really setuid root, and says SUBSTITUTED.
	nc sudo-step 'line=$(potluck doctor 2>/dev/null | sed -n "s/^ *\(sudo sh -c .*\)$/\1/p" | head -n 1)
		[ -n "$line" ] || { echo "doctor printed no root step"; exit 0; }
		echo "doctor'"'"'s root step: $line"
		eval "$line" && exit 0
		case $line in *softnet*) ;; *) echo "BLOCKED: the root step failed and is not the Softnet one"; exit 1 ;; esac
		f=/usr/local/bin/softnet
		if [ -u "$f" ] && [ "$(stat -f %Su "$f")" = root ]; then
			echo "SUBSTITUTED: the root step needs a password; linked this host'"'"'s setuid-root $f in its place"
			ln -sf "$f" ~/.local/bin/softnet
		else
			echo "BLOCKED: the root step needs a password and this host has no setuid-root $f to stand in"
			exit 1
		fi'
	nc doctor-3 'potluck doctor'
	nc create 'potluck create trial --via tunnel' || exit 1
	nc --bg host 'potluck host --reclaim-idle-s 0'
	# Wait until the step has ended (its .done file), then take its own exit code.
	while :; do
		nc --wait host 540
		rc=$?
		[ ! -e "$STRANGER_RUN/bg/host.done" ] || break
		kill -0 "$(cat "$STRANGER_RUN/bg/host.pid")" 2>/dev/null || break
	done
	[ "$rc" -eq 0 ] || exit 1
	out=$(nc sandbox-create 'potluck sandbox create') || exit 1
	echo "$out"
	id=$(printf '%s\n' "$out" | awk '/ ready /{print $1; exit}')
	[ -n "$id" ] || exit 1
	nc uname "potluck sandbox exec $id -- uname -a"
	nc rm "potluck sandbox rm $id"
}

main
