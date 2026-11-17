#!/bin/sh
# scripted-linux.sh: the README quick start on a fresh Ubuntu VM, step by step, as written. No agent: see
# scripted-mac.sh. Called by run.sh with STRANGER_RUN set.
set -u
nc() { "$STRANGER_RUN/nc" "$@"; }

# One function, read whole before it runs (see run.sh).
main() {

	# README "What you need" says Docker on Linux; the stock image has none. Ubuntu's own package is the plainest way.
	nc docker 'sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io >/dev/null && sudo usermod -aG docker "$USER" && docker --version'
	# A new group takes effect at the next login. The harness's stand-in for logging out and in again: restart the
	# VM's guest agent, so the next `tart exec` starts a session that has the docker group.
	nc relogin '(sleep 1; sudo systemctl restart tart-guest-agent) >/dev/null 2>&1 & echo "logging in again (guest agent restart)"'
	sleep 8
	nc install 'curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh' || exit 1
	nc path 'command -v potluck || { echo "export PATH=\"\$HOME/.local/bin:\$PATH\"" >> ~/.bashrc; export PATH="$HOME/.local/bin:$PATH"; command -v potluck; }'
	nc doctor-1 'potluck doctor'
	# The root step doctor prints (one `sudo sh -c ...` line). Cirrus's stock image gives its user passwordless sudo,
	# so here it runs as printed; report.md says so.
	nc sudo-step 'line=$(potluck doctor 2>/dev/null | sed -n "s/^ *\(sudo sh -c .*\)$/\1/p" | head -n 1); [ -n "$line" ] || { echo "doctor printed no sudo line"; exit 0; }; echo "$line"; eval "$line"'
	nc doctor-2 'potluck doctor'
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
