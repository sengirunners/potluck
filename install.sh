#!/bin/sh
# install.sh: install Potluck from its newest signed release.
#
#   curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
#   sh install.sh --version potluck-main-<12 hex>   # a given release, not the newest
#   sh install.sh --from <dir>     # a release directory you already have (air-gapped hosts)
#   sh install.sh --menubar        # on a Mac, also install Potluck Menu.app (docs/menubar.md), off unless asked
#   curl -fsSL https://potluck.sh/install.sh | sh -s -- --join '<invite link or code>'   # install, then join that pool
#
# When the install directory is not on PATH, it adds one line to the shell's startup file (~/.zshrc, ~/.bashrc,
# ~/.bash_profile, ~/.profile, or fish's conf.d/potluck.fish), so a new terminal finds potluck; --no-modify-path
# leaves every startup file alone and says what to add instead.
#
# It needs no Go. It gets the release for this machine (from GitHub, or from --from) and checks it before any
# program from it runs. Every release has a manifest naming each tarball's SHA-256, signed with the release key
# twice: an SSHSIG signature (manifest.json.sshsig) and a raw Ed25519 one (manifest.json.sig). This script checks
# the manifest against the release keys below:
#
#   - with `ssh-keygen -Y verify` (OpenSSH 8.1 or later: macOS 10.15 on, and Linux with openssh-client), else
#   - with `openssl pkeyutl -verify -rawin` (OpenSSL 3 or later; macOS's LibreSSL cannot check Ed25519).
#
# A tool counts only if it verifies a built-in probe signature and refuses that probe altered, and it must also
# refuse the release's manifest with one byte added: a tool that says yes to anything is never trusted. Then the
# tarball must have the SHA-256 the verified manifest names. A manifest that does not verify, or an unsigned
# release, is refused. If potluck is already installed ($POTLUCK_INSTALL_DIR/potluck, else potluck on PATH), it
# installs the release (`potluck upgrade --release`), checking the raw signature again with the keys built into it;
# otherwise the new potluck places the files after the checks above.
#
# With no tool here that can check the signature, it installs nothing and says what to install, unless potluck is
# already installed (that potluck's check is then the one made). --unsafe-skip-signature-check installs a download
# from GitHub over HTTPS anyway, with a warning.
#
# Environment:
#   POTLUCK_INSTALL_DIR  where the programs go (default ~/.local/bin)
#   POTLUCK_MENUBAR_DIR  where --menubar puts Potluck Menu.app (default ~/Applications)
#   GH_TOKEN, GITHUB_TOKEN  a token: the release is then read through GitHub's API with it (a private repository needs
#                        one). Without a token the script takes no API call: it downloads from github.com's release
#                        links, which GitHub's anonymous API limit (60 calls an hour per address, shared by everyone
#                        behind one office, CI or cloud NAT) does not count.
#   POTLUCK_UPDATE_KEY   another trusted release key (ed25519:<base64>), as config update_keys adds to potluck's
#   POTLUCK_GITHUB_API   GitHub's API (default https://api.github.com; tests)
#   POTLUCK_GITHUB       GitHub's web host, for the release links (default https://github.com; tests)
#   POTLUCK_RETRY_DELAY  whole seconds before the first retry of a dropped download, doubling after each (default 2; tests)
set -eu

# RELEASE_KEYS are the release keys this script trusts, separated by spaces: the list in
# internal/upgrade/release_keys.txt (a test holds them equal). During a key rotation it holds two
# (docs/release-keys.md). ssh-keygen gets the same keys as allowed_signers lines, made from these.
RELEASE_KEYS="ed25519:Z9tq+TU7lP6IwxiY2qzZC+bRNEv1G1fzxlLJYHtvTMc="
REPO="sengirunners/potluck"
# The SSHSIG principal and namespace (kit/release.Potluck.Namespace): a signature the release key made for
# anything else (a git commit, a file) never verifies as a release.
PRINCIPAL="potluck-release"
NAMESPACE="potluck-release"

# A throwaway key's signatures of PROBE_MSG (plus a newline), raw and SSHSIG: a tool counts only if it verifies
# these and refuses them over another message. OpenSSL 1.1.1 and LibreSSL fail the raw probe; an ssh-keygen older
# than OpenSSH 8.1 fails the SSHSIG one. (scripts/release/install_test.go checks them.)
PROBE_KEY="Pcoj7oZ08n9RkIWdzV/B4rIPZ7vUHMCG8Qls5i66Lsc="
PROBE_SIG="XyU0MQ+H0DvwYmaxNnslPNgjfa/+muwv8j1AS7+IRs1aBNHIr+rmH95+4Fi9Iw1ZFXaFggwyAmdo7GuqfPE7BQ=="
PROBE_SSHSIG="-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAgPcoj7oZ08n9RkIWdzV/B4rIPZ7
vUHMCG8Qls5i66LscAAAAPcG90bHVjay1yZWxlYXNlAAAAAAAAAAZzaGE1MTIAAABTAAAA
C3NzaC1lZDI1NTE5AAAAQGbM7os6n93KaS6DMYG6HB5lzWuQ8wfmDgQ7dvjGdBmrbp33J7
ppa9eQFggPkzXfrPTSIBn65wxFdUZ2nZ0nlAo=
-----END SSH SIGNATURE-----"
PROBE_MSG="potluck install.sh signature probe"

say() { printf '%s\n' "$*"; }
die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

usage() {
	say "usage: install.sh [--join <invite code>] [--version <release>] [--from <release dir>] [--menubar] [--no-modify-path] [--unsafe-skip-signature-check]"
	say "Installs potluck into \$POTLUCK_INSTALL_DIR (default ~/.local/bin), checking the release's signature first."
	say "--join joins the pool of that invite link or code once potluck is installed (potluck join)."
	say "--no-modify-path leaves shell startup files alone (by default a line putting the install directory on PATH is added)."
	say "--version takes a release (potluck-main-<12 hex>) instead of the newest."
	say "--from installs from a directory holding a release's files (its manifest, signatures and tarball)."
	say "--menubar also installs Potluck Menu.app into \$POTLUCK_MENUBAR_DIR (default ~/Applications); macOS only."
	say "--unsafe-skip-signature-check installs a download from GitHub even when no tool here can check its signature,"
	say "  trusting HTTPS alone (not with --from). A signature that does not verify is refused even then."
}

# fetch URL OUT ACCEPT [NOT_FOUND]: download URL into OUT over HTTPS, redirects included; a refusal says why, and a
# 404 says NOT_FOUND when it is given, or returns 1 when NOT_FOUND is "optional". A 5xx answer, or a curl error that
# a second try can cure (DNS, connect, reset, timeout), is tried again, FETCH_TRIES times in all, waiting longer each
# time; so is any wget failure but an optional file's (wget cannot say which kind it was). The token goes to curl on
# stdin, never on its command line.
FETCH_TRIES=4
fetch() {
	tries=0
	delay=${POTLUCK_RETRY_DELAY:-2}
	case $delay in "" | *[!0-9]*) delay=2 ;; esac
	while :; do
		tries=$((tries + 1))
		fetch_once "$@" || return 1
		case "$code" in 000 | 5??) ;; *) break ;; esac
		[ "$transient" = yes ] || [ "$code" != 000 ] || break
		[ "$tries" -lt "$FETCH_TRIES" ] || break
		say "install.sh: downloading $1 failed (try $tries of $FETCH_TRIES); trying again in ${delay}s" >&2
		sleep "$delay"
		delay=$((delay * 2))
	done
	fetch_answer "$@"
}

# fetch_once URL OUT ACCEPT [NOT_FOUND]: one try; sets code to the HTTP status, 000 when no answer came, and
# transient to yes when another try may get one. It returns 1 only for an optional file wget could not get (wget
# cannot tell a missing file from a network error).
fetch_once() {
	transient=yes
	if command -v curl >/dev/null 2>&1; then
		rc=0
		if [ -n "$TOKEN" ]; then
			code=$(printf 'Authorization: Bearer %s\n' "$TOKEN" |
				curl -sSL --proto '=https' --proto-redir '=https' --tlsv1.2 -o "$2" -w '%{http_code}' -H "Accept: $3" -H @- "$1") || rc=$?
		else
			code=$(curl -sSL --proto '=https' --proto-redir '=https' --tlsv1.2 -o "$2" -w '%{http_code}' -H "Accept: $3" "$1") || rc=$?
		fi
		if [ "$rc" != 0 ]; then
			code=000
			# curl's exits for DNS, connect, partial transfer, timeout, TLS handshake, empty reply, send, receive and
			# HTTP/2 stream errors; any other (a bad certificate, an option this curl lacks) fails the same way again.
			case $rc in 6 | 7 | 18 | 28 | 35 | 52 | 55 | 56 | 92) ;; *) transient=no ;; esac
		fi
	elif command -v wget >/dev/null 2>&1; then
		# wget would put the token on its command line, and send it on to the storage host GitHub redirects
		# downloads to: a token needs curl.
		[ -z "$TOKEN" ] || die "downloading with GH_TOKEN/GITHUB_TOKEN needs curl"
		code=000
		if wget -q $WGET_HTTPS -O "$2" --header="Accept: $3" "$1"; then code=200; fi
		if [ "$code" = 000 ] && [ "${4:-}" = optional ]; then
			rm -f "$2"
			WGET_MISSED="$WGET_MISSED ${2##*/}"
			return 1
		fi
	else
		die "need curl or wget to download potluck"
	fi
}

# fetch_answer URL OUT ACCEPT [NOT_FOUND]: what the last try's code means.
fetch_answer() {
	case "$code" in
	200) return 0 ;;
	404)
		if [ "${4:-}" = optional ]; then
			rm -f "$2"
			return 1
		fi
		[ -z "${4:-}" ] || die "$4"
		if [ -z "$TOKEN" ]; then
			die "github.com/$REPO answered 404 with no token: it is private (set GH_TOKEN or GITHUB_TOKEN to a token that can read it) or has no such release. Or use --from <release dir>."
		fi
		die "github.com/$REPO answered 404: the token in GH_TOKEN/GITHUB_TOKEN cannot read it (or it has no such release)."
		;;
	401) die "github.com/$REPO answered 401: the token in GH_TOKEN/GITHUB_TOKEN is not valid." ;;
	403 | 429)
		if [ -n "$TOKEN" ]; then
			die "github.com/$REPO answered $code: the token in GH_TOKEN/GITHUB_TOKEN is rate limited or refused. Try again later."
		fi
		die "github.com answered $code for $1: rate limited or refused. Try again later."
		;;
	000)
		# curl has printed why above; wget says nothing, and cannot tell a missing file from a dropped connection.
		if command -v curl >/dev/null 2>&1; then
			[ "$transient" = yes ] || die "could not download $1 (curl's error is above)"
			die "could not download $1 after $tries tries (curl's error is above). Run install.sh again, or download the release's files and use --from <dir>."
		fi
		die "could not download $1 with wget after $tries tries (not found, refused, or the connection dropped; curl would say which)"
		;;
	5??) die "github.com answered $code for $1 after $tries tries: a fault on GitHub's side. Run install.sh again later." ;;
	*) die "github.com/$REPO answered $code for $1" ;;
	esac
}

# assets FILE: "<name> <API url>" for each asset in a GitHub release (JSON): every object that has a name and a
# url ending /releases/assets/<id>, keys in any order. (Braces inside strings would confuse it; a release's own
# text has none.)
assets() {
	tr ',' '\n' <"$1" | sed 's/{/\
{\
/g; s/}/\
}\
/g' | awk '
		$0 == "{" { d++; u[d] = ""; n[d] = ""; next }
		$0 == "}" { if (u[d] != "" && n[d] != "") print n[d], u[d]; d--; next }
		/^[[:space:]]*"url"[[:space:]]*:/ { v = $0; sub(/^[^:]*:[[:space:]]*"/, "", v); sub(/".*$/, "", v); if (v ~ /\/releases\/assets\/[0-9]+$/) u[d] = v; next }
		/^[[:space:]]*"name"[[:space:]]*:/ { v = $0; sub(/^[^:]*:[[:space:]]*"/, "", v); sub(/".*$/, "", v); n[d] = v }'
}

# manifest_version FILE: the version a release manifest names (letters, digits, . _ -), else nothing.
manifest_version() {
	sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([A-Za-z0-9._-]*\)".*/\1/p' "$1" | head -n 1
}

# sha256 FILE: its SHA-256 in hex.
sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$1" | awk '{ print $NF }'
	else
		die "need sha256sum, shasum or openssl to check the download"
	fi
}

# b64d, b64e: base64 decode and encode (one line) on stdin, with base64(1) or else openssl (LibreSSL does this
# much). B64 is empty when neither works.
pick_base64() {
	B64=""
	if [ "$(printf 'cG90bHVjaw==' | base64 -d 2>/dev/null)" = potluck ]; then
		B64=base64
	elif command -v openssl >/dev/null 2>&1 && [ "$(printf 'cG90bHVjaw==' | openssl base64 -d -A 2>/dev/null)" = potluck ]; then
		B64=openssl
	fi
}
b64d() { if [ "$B64" = base64 ]; then base64 -d; else openssl base64 -d -A; fi; }
b64e() { if [ "$B64" = base64 ]; then base64 | tr -d '\n'; else openssl base64 -A; fi; }

# signer_line KEY_B64: the allowed_signers line for a raw Ed25519 key: the key in SSH wire format (the string
# "ssh-ed25519", then the 32 key bytes, each length-prefixed), limited to the release namespace.
signer_line() {
	printf '%s' "$1" | b64d >"$tmp/key.bin" 2>/dev/null || return 1
	[ "$(wc -c <"$tmp/key.bin" | tr -d ' ')" = 32 ] || return 1
	w=$({
		printf '\000\000\000\013ssh-ed25519\000\000\000\040'
		cat "$tmp/key.bin"
	} | b64e) || return 1
	printf '%s namespaces="%s" ssh-ed25519 %s\n' "$PRINCIPAL" "$NAMESPACE" "$w"
}

# sshsig_verify SIGNERS SIG FILE: whether SIG is an SSHSIG signature of FILE by a key in SIGNERS (allowed_signers).
sshsig_verify() {
	ssh-keygen -Y verify -f "$1" -I "$PRINCIPAL" -n "$NAMESPACE" -s "$2" <"$3" >/dev/null 2>&1
}

# can_sshsig: whether this machine's ssh-keygen checks SSHSIG signatures: it verifies the probe, and refuses it
# over another message (an ssh-keygen that always succeeds is not trusted).
can_sshsig() {
	command -v ssh-keygen >/dev/null 2>&1 && [ -n "$B64" ] || return 1
	signer_line "$PROBE_KEY" >"$tmp/probe_signers" || return 1
	say "$PROBE_SSHSIG" >"$tmp/probe.sshsig"
	say "$PROBE_MSG" >"$tmp/probe.txt"
	say "$PROBE_MSG, changed" >"$tmp/probe-bad.txt"
	sshsig_verify "$tmp/probe_signers" "$tmp/probe.sshsig" "$tmp/probe.txt" || return 1
	! sshsig_verify "$tmp/probe_signers" "$tmp/probe.sshsig" "$tmp/probe-bad.txt"
}

# ed25519_verify KEY_B64 FILE SIG_B64: whether SIG_B64 is KEY_B64's signature of FILE (openssl).
ed25519_verify() {
	{
		say "-----BEGIN PUBLIC KEY-----"
		# The SubjectPublicKeyInfo of an Ed25519 key is this 12-byte prefix, then the 32-byte key.
		{
			printf '\060\052\060\005\006\003\053\145\160\003\041\000'
			printf '%s' "$1" | openssl base64 -d -A
		} | openssl base64 -A
		say ""
		say "-----END PUBLIC KEY-----"
	} >"$tmp/key.pem" 2>/dev/null || return 1
	printf '%s' "$3" | tr -d ' \n\r' | openssl base64 -d -A >"$tmp/sig.bin" 2>/dev/null || return 1
	openssl pkeyutl -verify -pubin -inkey "$tmp/key.pem" -rawin -in "$2" -sigfile "$tmp/sig.bin" >/dev/null 2>&1
}

# can_verify: whether this machine's openssl checks Ed25519 signatures: it verifies the probe, and refuses it over
# another message (an openssl that always succeeds is not trusted).
can_verify() {
	command -v openssl >/dev/null 2>&1 || return 1
	say "$PROBE_MSG" >"$tmp/probe.txt"
	say "$PROBE_MSG, changed" >"$tmp/probe-bad.txt"
	ed25519_verify "$PROBE_KEY" "$tmp/probe.txt" "$PROBE_SIG" || return 1
	! ed25519_verify "$PROBE_KEY" "$tmp/probe-bad.txt" "$PROBE_SIG"
}

# check_signature DIR: 0 when manifest.json verifies with a trusted key (how in $checked); 2 when no tool here can
# check it (why in $why, what to install in $fix). Anything else ends the script: an empty or malformed key list,
# an unsigned release, a signature that does not verify, or a tool that also accepts an altered manifest.
check_signature() {
	keys="$RELEASE_KEYS ${POTLUCK_UPDATE_KEY:-}"
	[ -n "$(printf '%s' "$RELEASE_KEYS" | tr -d ' ')" ] || die "this install.sh trusts no release key (RELEASE_KEYS is empty): not installing"
	for k in $keys; do
		case "$k" in
		ed25519:*[!A-Za-z0-9+/=]* | ed25519:) die "release key \"$k\" is not ed25519:<base64>: not installing" ;;
		ed25519:*) ;;
		*) die "release key \"$k\" is not ed25519:<base64>: not installing" ;;
		esac
	done
	[ -s "$1/manifest.json.sshsig" ] || [ -s "$1/manifest.json.sig" ] ||
		die "the release has no signature (manifest.json.sshsig or manifest.json.sig): not installing"
	cat "$1/manifest.json" >"$tmp/altered.json"
	printf ' ' >>"$tmp/altered.json"
	pick_base64
	tried=""
	if [ -s "$1/manifest.json.sshsig" ] && can_sshsig; then
		tried="ssh-keygen"
		: >"$tmp/allowed_signers"
		for k in $keys; do
			signer_line "${k#ed25519:}" >>"$tmp/allowed_signers" || die "release key \"$k\" is not an Ed25519 key: not installing"
		done
		if sshsig_verify "$tmp/allowed_signers" "$1/manifest.json.sshsig" "$1/manifest.json"; then
			! sshsig_verify "$tmp/allowed_signers" "$1/manifest.json.sshsig" "$tmp/altered.json" ||
				die "ssh-keygen here accepts an altered manifest, so it cannot be trusted to check this one: not installing"
			checked="SSHSIG, checked with ssh-keygen"
			return 0
		fi
	fi
	if [ -s "$1/manifest.json.sig" ] && can_verify; then
		tried="${tried:+$tried and }openssl"
		sig=$(cat "$1/manifest.json.sig")
		for k in $keys; do
			if ed25519_verify "${k#ed25519:}" "$1/manifest.json" "$sig"; then
				! ed25519_verify "${k#ed25519:}" "$tmp/altered.json" "$sig" ||
					die "openssl here accepts an altered manifest, so it cannot be trusted to check this one: not installing"
				checked="Ed25519, checked with OpenSSL"
				return 0
			fi
		done
	fi
	if [ -n "$tried" ]; then
		die "the release's manifest does not verify with any release key (checked with $tried): not installing. The download may have been tampered with; try again, and if it repeats, report it."
	fi
	if [ ! -s "$1/manifest.json.sshsig" ]; then
		case " $WGET_MISSED " in
		*" manifest.json.sshsig "*)
			why="could not download manifest.json.sshsig with wget (not found, refused, or no network) and no openssl here can check the Ed25519 signature"
			fix="run this again, install curl (which says why a download fails), or put an OpenSSL 3 or newer first on PATH"
			return 2
			;;
		esac
		why="this release has no SSHSIG signature (manifest.json.sshsig) and no openssl here can check its Ed25519 one"
		fix="run this again once the next release is out (every new release carries one), or put an OpenSSL 3 or newer first on PATH"
	elif command -v ssh-keygen >/dev/null 2>&1; then
		why="this ssh-keygen cannot check SSHSIG signatures (it needs OpenSSH 8.1 or newer) and no openssl here can check Ed25519 ones"
		fix="install OpenSSH 8.1 or newer, or put an OpenSSL 3 or newer first on PATH"
	else
		why="neither ssh-keygen (OpenSSH) nor an OpenSSL 3 or newer is installed"
		fix="install OpenSSH's ssh-keygen (Debian, Ubuntu: sudo apt-get install openssh-client; Fedora: sudo dnf install openssh-clients; Alpine: apk add openssh-keygen) and run this again"
	fi
	return 2
}

# installed_potluck: the potluck already installed that can install a release (it has upgrade), else nothing.
# A potluck found on PATH counts only at an absolute path outside the release's directory and this script's
# temporary one (a PATH with an empty or "." entry would otherwise find the release's own program).
installed_potluck() {
	p=""
	if [ -x "$INSTALL_DIR/potluck" ]; then
		p="$INSTALL_DIR/potluck"
	elif command -v potluck >/dev/null 2>&1; then
		p=$(command -v potluck)
		case "$p" in
		/*) ;;
		*) p="" ;;
		esac
		if [ -n "$p" ]; then
			pd=$(cd "$(dirname "$p")" 2>/dev/null && pwd -P) || pd=""
			tp=$(cd "$tmp" && pwd -P)
			case "$pd/" in
			/ | "$tp"/* | "${src:-$tp}"/*) p="" ;;
			esac
		fi
	fi
	if [ -n "$p" ] && POTLUCK_NO_AUTOSTART=1 "$p" upgrade --help >/dev/null 2>&1; then
		say "$p"
	fi
}

# menu_running APP: whether this user runs the Potluck Menu at APP.
menu_running() {
	# The command must start with the path, so this pipeline's own awk never matches.
	ps -u "$(id -u)" -o command= 2>/dev/null | awk -v p="$1/Contents/MacOS/PotluckMenu" 'index($0, p) == 1 { f = 1 } END { exit !f }'
}

# install_menubar DIR: put Potluck Menu.app from the release's darwin tarball (already checked, but its hash is
# compared with the manifest's again here) into $POTLUCK_MENUBAR_DIR (default ~/Applications). The app's build is not Developer ID signed yet, and
# the script says so when that is the case.
install_menubar() {
	[ "$os" = darwin ] || die "--menubar is for macOS: Potluck Menu.app is a Mac menu bar item"
	command -v ditto >/dev/null 2>&1 || die "--menubar needs ditto (macOS)"
	mv_version=$(manifest_version "$1/manifest.json")
	mv_tarball="potluck_${mv_version}_${os}_${arch}.tar.gz"
	mv_want=$(awk -v f="\"$mv_tarball\":" '$1 == f { v = $2; gsub(/[",]/, "", v); print v; exit }' "$1/manifest.json")
	[ -n "$mv_want" ] && [ "$(sha256 "$1/$mv_tarball")" = "$mv_want" ] || die "$mv_tarball does not match the manifest: not installing the menu bar item"
	mv_entry="potluck_${mv_version}_${os}_${arch}/Potluck-Menu.zip"
	tar -tzf "$1/$mv_tarball" 2>/dev/null | grep -qx "$mv_entry" || die "release $mv_version has no Potluck Menu.app (potluck itself is installed)"
	mkdir "$tmp/menubar"
	tar -xzf "$1/$mv_tarball" -C "$tmp/menubar" "$mv_entry" || die "could not unpack $mv_entry from $mv_tarball"
	mkdir "$tmp/menubar/app"
	ditto -x -k "$tmp/menubar/$mv_entry" "$tmp/menubar/app" || die "could not unpack Potluck Menu.app"
	[ -d "$tmp/menubar/app/Potluck Menu.app" ] || die "the release's Potluck-Menu.zip holds no Potluck Menu.app"
	mv_dir="${POTLUCK_MENUBAR_DIR:-${HOME:?HOME is not set}/Applications}"
	mkdir -p "$mv_dir" || die "could not create $mv_dir"
	mv_dir=$(cd "$mv_dir" && pwd -P) || die "could not enter $mv_dir" # ps shows the real path
	mv_dest="$mv_dir/Potluck Menu.app"
	# Replacing an installed copy: ask this copy (not another one on the Mac) to quit first, and do not swap it
	# while it still runs.
	if [ -d "$mv_dest" ] && menu_running "$mv_dest"; then
		say "quitting the running Potluck Menu to replace it; open it again afterwards"
		osascript -e "tell application \"$mv_dest\" to quit" >/dev/null 2>&1 || true
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			menu_running "$mv_dest" || break
			sleep 1
		done
		! menu_running "$mv_dest" || die "Potluck Menu is still running: quit it from its menu and run this again (potluck itself is installed)"
	fi
	# Copy beside the old one, then swap, so a failed copy leaves the old app in place.
	rm -rf "$mv_dir/.Potluck Menu.app.new"
	ditto "$tmp/menubar/app/Potluck Menu.app" "$mv_dir/.Potluck Menu.app.new" || die "could not place the app in $mv_dir"
	rm -rf "$mv_dest"
	mv "$mv_dir/.Potluck Menu.app.new" "$mv_dest" || die "could not place $mv_dest"
	say "installed $mv_dest"
	if codesign -dv --verbose=2 "$mv_dest" 2>&1 | grep -q '^Authority=Developer ID Application'; then
		say "it is signed with a Developer ID"
	else
		say "note: this build is not Developer ID signed (ad hoc only). It runs when installed this way; if macOS blocks it, right-click it in Finder and choose Open."
	fi
	say "  open it:       open \"$mv_dest\""
	say "  at every login: System Settings > General > Login Items > add Potluck Menu"
}

# path_setup: the startup file of the shell this person runs (macOS's zsh does not read ~/.profile) and the line that
# puts INSTALL_DIR on PATH there. It sets PATH_RC (an absolute path), PATH_RC_SHOWN (as a person writes it) and
# PATH_LINE; it returns 1 when there is no HOME, or the directory is relative or has characters a shell treats
# specially.
path_setup() {
	[ -n "${HOME:-}" ] || return 1
	case "$INSTALL_DIR" in
	/*) ;;
	*) return 1 ;;
	esac
	case "$INSTALL_DIR" in
	"${HOME:-/nonexistent}"/*)
		rest="${INSTALL_DIR#"${HOME:-}"/}"
		dir="\$HOME/$rest"
		;;
	*)
		rest="$INSTALL_DIR"
		dir="$INSTALL_DIR"
		;;
	esac
	case "$rest" in
	*[\'\"\`\$\\]*) return 1 ;;
	esac
	PATH_LINE="export PATH=\"$dir:\$PATH\""
	case "${SHELL:-}" in
	*/zsh)
		rc=.zshrc
		case "${ZDOTDIR:-}" in
		"$HOME"/*) rc="${ZDOTDIR#"$HOME"/}/.zshrc" ;;
		?*) return 1 ;; # a ZDOTDIR outside HOME: say the line instead
		esac
		;;
	*/bash)
		if [ "$(uname -s)" != Darwin ]; then
			rc=.bashrc
		elif [ -e "${HOME:-/nonexistent}/.bash_profile" ] || { [ ! -e "${HOME:-/nonexistent}/.bash_login" ] && [ ! -e "${HOME:-/nonexistent}/.profile" ]; }; then
			rc=.bash_profile
		elif [ -e "${HOME:-/nonexistent}/.bash_login" ]; then
			rc=.bash_login # bash reads only the first of .bash_profile, .bash_login and .profile that exists
		else
			rc=.profile
		fi
		;;
	*/fish)
		rc=.config/fish/conf.d/potluck.fish
		PATH_LINE="fish_add_path \"$dir\""
		;;
	*) rc=.profile ;;
	esac
	case "$rc" in
	*[\'\"\`\$\\]*) return 1 ;;
	esac
	PATH_RC="$HOME/$rc"
	PATH_RC_SHOWN="~/$rc"
	return 0
}

# put_on_path: unless INSTALL_DIR is on PATH already, add PATH_LINE to PATH_RC once (or, with --no-modify-path, as
# root, or a file it cannot write, say the line to add). A failure here never fails the install.
put_on_path() {
	case ":${PATH:-}:" in
	*":$INSTALL_DIR:"*) return 0 ;;
	esac
	say ""
	if ! path_setup; then
		say "$INSTALL_DIR is not on your PATH: add it to PATH yourself, so a new terminal finds potluck."
		return 0
	fi
	# As root (sudo keeps HOME on some systems) a startup file would become root's: say the line instead.
	if [ "$MODIFY_PATH" = 1 ] && [ "$(id -u 2>/dev/null || echo 1)" != 0 ]; then
		if [ -f "$PATH_RC" ] && grep -qxF "$PATH_LINE" "$PATH_RC" 2>/dev/null; then
			say "$PATH_RC_SHOWN already puts $INSTALL_DIR on PATH; a new terminal finds potluck."
			PATH_ADDED=1
			return 0
		fi
		if { mkdir -p "$(dirname "$PATH_RC")" &&
			printf '\n# Potluck (install.sh): put potluck on PATH\n%s\n' "$PATH_LINE" >>"$PATH_RC"; } 2>/dev/null; then
			say "added $INSTALL_DIR to PATH in $PATH_RC_SHOWN; a new terminal finds potluck."
			PATH_ADDED=1
			return 0
		fi
		say "could not write $PATH_RC_SHOWN. $INSTALL_DIR is not on your PATH; add it with:"
	else
		say "$INSTALL_DIR is not on your PATH, so a new terminal will not find potluck. Add it with:"
	fi
	say "  echo '$PATH_LINE' >> $PATH_RC_SHOWN"
}

next_steps() {
	put_on_path
	if [ -n "$JOIN" ]; then
		say ""
		say "joining the pool of your invite code"
		# stdin is the script itself under curl | sh: join must not read it.
		if ! "$INSTALL_DIR/potluck" join "$JOIN" </dev/null; then
			die "potluck is installed, but joining the pool failed (see above). Fix that, then run: \"$INSTALL_DIR/potluck\" join '<your invite code>'"
		fi
		if [ "$PATH_ADDED" = 1 ]; then
			say "(in this terminal, first run: export PATH=\"$INSTALL_DIR:\$PATH\"; a new terminal has it already)"
		fi
		return 0
	fi
	say ""
	say "Next:"
	say "  see what this machine can share:  potluck doctor"
	say "  start a pool:  potluck create <pool>      (then: potluck invite <name>)"
	say "  join a pool:   potluck join '<invite code>'"
	say "  keep potluck current, once a day:  potluck upgrade enable"
	if [ "$PATH_ADDED" = 1 ]; then
		say "  (in this terminal, first run: export PATH=\"$INSTALL_DIR:\$PATH\"; a new terminal has it already)"
	fi
}

main() {
	API="${POTLUCK_GITHUB_API:-https://api.github.com}"
	WEB="${POTLUCK_GITHUB:-https://github.com}"
	INSTALL_DIR="${POTLUCK_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}"
	TOKEN="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
	FROM=""
	VERSION=""
	VERSION_SET=0
	WGET_MISSED=""
	MENUBAR=0
	UNSAFE=0
	JOIN=""
	MODIFY_PATH=1
	PATH_ADDED=0
	while [ $# -gt 0 ]; do
		case "$1" in
		--from)
			[ $# -ge 2 ] || die "--from needs a directory"
			FROM="$2"
			shift 2
			;;
		--from=*)
			FROM="${1#--from=}"
			shift
			;;
		--version)
			[ $# -ge 2 ] || die "--version needs a release, like potluck-main-<12 hex>"
			VERSION="$2"
			VERSION_SET=1
			shift 2
			;;
		--version=*)
			VERSION="${1#--version=}"
			VERSION_SET=1
			shift
			;;
		--menubar)
			MENUBAR=1
			shift
			;;
		--join)
			[ $# -ge 2 ] && [ -n "$2" ] || die "--join needs the invite code the pool's owner sent you"
			JOIN="$2"
			shift 2
			;;
		--join=*)
			JOIN="${1#--join=}"
			[ -n "$JOIN" ] || die "--join needs the invite code the pool's owner sent you"
			shift
			;;
		--no-modify-path)
			MODIFY_PATH=0
			shift
			;;
		--unsafe-skip-signature-check)
			UNSAFE=1
			shift
			;;
		-h | --help)
			usage
			return 0
			;;
		*)
			usage >&2
			exit 2
			;;
		esac
	done
	# A release is tagged potluck-<version>; --version takes either form.
	VERSION="${VERSION#potluck-}"
	[ "$VERSION_SET" = 0 ] || [ -n "$VERSION" ] || die "--version needs a release, like potluck-main-<12 hex>"
	case "$VERSION" in
	*[!A-Za-z0-9._-]* | [._-]*) die "--version $VERSION is not a release (potluck-main-<12 hex>)" ;;
	esac
	[ "$UNSAFE" != 1 ] || [ -z "$FROM" ] ||
		die "--unsafe-skip-signature-check is only for a download from GitHub over HTTPS, not --from: a directory has nothing else vouching for it"

	# This machine's OS and architecture, as the release names them.
	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "potluck runs on macOS and Linux, not $(uname -s)" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "no potluck build for $(uname -m)" ;;
	esac
	# A shell under Rosetta reports x86_64 on an Apple-silicon Mac: take the native build.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || echo 0)" = 1 ]; then
		arch=arm64
	fi

	[ "$MENUBAR" != 1 ] || [ "$os" = darwin ] || die "--menubar is for macOS: Potluck Menu.app is a Mac menu bar item"

	# A wget without --https-only (BusyBox's) may follow a redirect to plain http: then the newest release is not
	# taken on its word (name one with --version, whose signed manifest must say so), and nothing is installed
	# unchecked.
	WGET_HTTPS=""
	if [ -z "$FROM" ] && ! command -v curl >/dev/null 2>&1 && command -v wget >/dev/null 2>&1; then
		if wget --help 2>&1 | grep -q -- '--https-only'; then
			WGET_HTTPS=--https-only
		else
			[ -n "$VERSION" ] || die "this wget cannot insist on HTTPS (BusyBox?): install curl, or name the release with --version"
			[ "$UNSAFE" != 1 ] || die "--unsafe-skip-signature-check needs curl or a wget that has --https-only"
		fi
	fi

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/potluck.XXXXXX" 2>/dev/null || mktemp -d -t potluck)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	dl="$tmp/release"
	mkdir "$dl"
	src=""
	# want: the release the files must be, once the manifest is verified: --version, or the tag GitHub's API named.
	want_version="$VERSION"
	if [ -n "$FROM" ]; then
		# Each file is read from --from exactly once, into this script's own (0700) directory; every later step
		# reads only those copies, so a directory that changes underneath cannot swap what was checked.
		[ -d "$FROM" ] || die "--from $FROM is not a directory"
		src=$(cd "$FROM" && pwd -P)
		[ -e "$src/manifest.json" ] || die "$src has no manifest.json: not a potluck release directory"
		cat "$src/manifest.json" >"$dl/manifest.json" || die "could not read $src/manifest.json"
		for f in manifest.json.sshsig manifest.json.sig; do
			if [ -e "$src/$f" ]; then
				cat "$src/$f" >"$dl/$f" || die "could not read $src/$f"
			fi
		done
		v=$(manifest_version "$dl/manifest.json")
		[ -n "$v" ] || die "the manifest names no version"
		t="potluck_${v}_${os}_${arch}.tar.gz"
		[ -e "$src/$t" ] || die "release $v has no build for $os/$arch ($t)"
		cat "$src/$t" >"$dl/$t" || die "could not read $src/$t"
	elif [ -z "$TOKEN" ]; then
		# No token: github.com's release links, which take no API call. The release is tagged potluck-<version>; the
		# newest one's manifest names its version, and the signatures and the tarball come from that tag, so a
		# release published meanwhile cannot mix two releases' files.
		if [ -n "$VERSION" ]; then
			fetch "$WEB/$REPO/releases/download/potluck-$VERSION/manifest.json" "$dl/manifest.json" "application/octet-stream" \
				"github.com/$REPO has no release potluck-$VERSION"
		else
			fetch "$WEB/$REPO/releases/latest/download/manifest.json" "$dl/manifest.json" "application/octet-stream"
		fi
		v=$(manifest_version "$dl/manifest.json")
		[ -n "$v" ] || die "the ${VERSION:-newest} release of $REPO has a manifest that names no version, not a potluck release"
		[ -n "$VERSION" ] || want_version="$v"
		t="potluck_${v}_${os}_${arch}.tar.gz"
		fetch "$WEB/$REPO/releases/download/potluck-$want_version/manifest.json.sshsig" "$dl/manifest.json.sshsig" "application/octet-stream" optional || true
		fetch "$WEB/$REPO/releases/download/potluck-$want_version/manifest.json.sig" "$dl/manifest.json.sig" "application/octet-stream" optional || true
		fetch "$WEB/$REPO/releases/download/potluck-$want_version/$t" "$dl/$t" "application/octet-stream" \
			"release potluck-$want_version has no build for $os/$arch ($t)"
	else
		if [ -n "$VERSION" ]; then
			fetch "$API/repos/$REPO/releases/tags/potluck-$VERSION" "$tmp/latest.json" "application/vnd.github+json"
		else
			fetch "$API/repos/$REPO/releases/latest" "$tmp/latest.json" "application/vnd.github+json"
		fi
		tag=$(tr ',{}' '\n\n\n' <"$tmp/latest.json" | sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
		case "$tag" in
		potluck-?*) ;;
		*) die "the ${VERSION:-newest} release of $REPO is \"$tag\", not a potluck release" ;;
		esac
		want_version="${tag#potluck-}"
		assets "$tmp/latest.json" >"$tmp/assets"
		for f in manifest.json manifest.json.sshsig manifest.json.sig "potluck_${want_version}_${os}_${arch}.tar.gz"; do
			url=$(awk -v f="$f" '$1 == f { print $2; exit }' "$tmp/assets")
			if [ -z "$url" ]; then
				case "$f" in
				*.sshsig | *.sig) continue ;; # check_signature says which a release lacks
				esac
				die "release $tag has no $f (no build for $os/$arch?)"
			fi
			fetch "$url" "$dl/$f" "application/octet-stream"
		done
	fi

	# The signature is checked here, before anything from the release runs, whether or not potluck is installed.
	installed=$(installed_potluck)
	why=""
	fix=""
	checked=""
	if check_signature "$dl"; then
		say "signature verified with a release key ($checked)"
	elif [ -n "$installed" ]; then
		say "this script cannot check the signature here ($why); the potluck already installed checks it with the keys built into it"
	elif [ "$UNSAFE" = 1 ]; then
		say "WARNING: THE RELEASE SIGNATURE WAS NOT CHECKED ($why)."
		say "WARNING: --unsafe-skip-signature-check: trusting the download from github.com over HTTPS alone."
	else
		msg="cannot check the release's signature here: $why. Not installing.
  Fix: $fix."
		[ -n "$FROM" ] || msg="$msg
  To install without the check, trusting the HTTPS download alone, add --unsafe-skip-signature-check."
		die "$msg"
	fi

	# The version, schema and the tarball's hash come from the manifest (verified above, unless unsafe).
	version=$(manifest_version "$dl/manifest.json")
	[ -n "$version" ] || die "the manifest names no version"
	grep -q '"schema":[[:space:]]*"potluck-release/1"' "$dl/manifest.json" || die "the manifest is not a potluck-release/1 manifest"
	if [ -n "$want_version" ] && [ "$version" != "$want_version" ]; then
		die "asked for release potluck-$want_version, but the signed manifest is release potluck-$version: not installing"
	fi

	# An installed potluck installs the release, checking it again with its own keys; nothing from the release runs
	# first.
	if [ -n "$installed" ]; then
		say "installing the release with the potluck already installed ($installed)"
		if ! POTLUCK_NO_AUTOSTART=1 "$installed" upgrade --release "$dl" --dir "$INSTALL_DIR"; then
			die "potluck did not install (see above); nothing was changed"
		fi
		[ "$MENUBAR" != 1 ] || install_menubar "$dl"
		next_steps
		return 0
	fi

	tarball="potluck_${version}_${os}_${arch}.tar.gz"
	[ -f "$dl/$tarball" ] || die "release $version has no build for $os/$arch ($tarball)"
	want=$(awk -v f="\"$tarball\":" '$1 == f { v = $2; gsub(/[",]/, "", v); print v; exit }' "$dl/manifest.json")
	[ -n "$want" ] || die "the manifest names no $tarball"
	got=$(sha256 "$dl/$tarball")
	[ "$got" = "$want" ] || die "$tarball does not match the signed manifest (sha256 $got, manifest $want): not installing"
	say "installing potluck $version for $os/$arch"

	mkdir "$tmp/unpacked"
	tar -xzf "$dl/$tarball" -C "$tmp/unpacked" || die "could not unpack $tarball"
	new="$tmp/unpacked/potluck_${version}_${os}_${arch}/potluck"
	if [ ! -f "$new" ] || [ ! -x "$new" ]; then
		die "$tarball holds no potluck program"
	fi
	# The new potluck places the files (each replaced one kept as .prev) after checking them again.
	if ! POTLUCK_NO_AUTOSTART=1 "$new" upgrade --release "$dl" --dir "$INSTALL_DIR"; then
		die "potluck did not install (see above); nothing was changed"
	fi
	[ "$MENUBAR" != 1 ] || install_menubar "$dl"
	next_steps
}

main "$@"
