#!/bin/sh
# install.sh: install Potluck from its newest signed release.
#
#   curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
#   sh install.sh --from <dir>     # a release directory you already have (air-gapped hosts)
#   sh install.sh --menubar        # on a Mac, also install Potluck Menu.app (docs/menubar.md), off unless asked
#
# It needs no Go. It gets the release for this machine (from GitHub, or from --from) and, except in the last case
# below, never runs a program from it before its signature is checked by something the download does not control:
#
#   - If potluck is already installed ($POTLUCK_INSTALL_DIR/potluck, else potluck on PATH), that potluck installs
#     it: `potluck upgrade --release` checks the manifest's Ed25519 signature with the key built into it and the
#     tarball against the signed manifest. The new programs are never run first. This is only as good as that
#     potluck's own first install.
#   - Otherwise (a first install) this script checks the signature itself, with the release keys below and
#     `openssl pkeyutl -verify -rawin` (an openssl that can do Ed25519: OpenSSL 3, not macOS's LibreSSL), then the
#     tarball's SHA-256 against the hash inside the signed manifest. Only then is it unpacked and installed (the
#     new potluck places the files).
#   - With no installed potluck and no such openssl, a download from GitHub is trusted over HTTPS alone, and the
#     script says that the signature was not checked. A --from directory is refused unless you pass --trust-dir.
#
# Environment:
#   POTLUCK_INSTALL_DIR  where the programs go (default ~/.local/bin)
#   POTLUCK_MENUBAR_DIR  where --menubar puts Potluck Menu.app (default ~/Applications)
#   GH_TOKEN, GITHUB_TOKEN  a token sent with every GitHub request: needed if the repository is private; also lifts the rate limit
#   POTLUCK_UPDATE_KEY   another trusted release key (ed25519:<base64>), as config update_keys adds to potluck's
#   POTLUCK_GITHUB_API   GitHub's API (default https://api.github.com; tests)
set -eu

# RELEASE_KEYS are the release keys this script trusts, separated by spaces: the list in
# internal/upgrade/release_keys.txt (a test holds them equal). During a key rotation it holds two
# (docs/release-keys.md).
RELEASE_KEYS="ed25519:Z9tq+TU7lP6IwxiY2qzZC+bRNEv1G1fzxlLJYHtvTMc="
REPO="sengirunners/potluck"

# A known Ed25519 signature: an openssl that verifies it can check releases.
PROBE_KEY="EAFL1w8YsYTHEzxxNSDehKoog8guGYONSlbvKUcUHoI="
PROBE_SIG="nhHl6R/+lb1vtwRobzNh1j9QFJJeKKD7jeZ66O+FRkP287r1+abKSfur0rY2CXjvVq/3a5lzbFoloBf1l30YDw=="
PROBE_MSG="potluck openssl ed25519 probe"

say() { printf '%s\n' "$*"; }
die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

usage() {
	say "usage: install.sh [--from <release dir> [--trust-dir]] [--menubar]"
	say "Installs potluck into \$POTLUCK_INSTALL_DIR (default ~/.local/bin)."
	say "--menubar also installs Potluck Menu.app into \$POTLUCK_MENUBAR_DIR (default ~/Applications); macOS only."
	say "--trust-dir installs from --from without a signature check when none can be made here."
}

# fetch URL OUT ACCEPT: download URL into OUT over HTTPS; a refusal says why. The token goes to curl on stdin,
# never on its command line.
fetch() {
	if command -v curl >/dev/null 2>&1; then
		if [ -n "$TOKEN" ]; then
			code=$(printf 'Authorization: Bearer %s\n' "$TOKEN" |
				curl -sSL --proto '=https' --tlsv1.2 -o "$2" -w '%{http_code}' -H "Accept: $3" -H @- "$1") || code=000
		else
			code=$(curl -sSL --proto '=https' --tlsv1.2 -o "$2" -w '%{http_code}' -H "Accept: $3" "$1") || code=000
		fi
	elif command -v wget >/dev/null 2>&1; then
		# wget would put the token on its command line, and send it on to the storage host GitHub redirects
		# downloads to: a token needs curl.
		[ -z "$TOKEN" ] || die "downloading with GH_TOKEN/GITHUB_TOKEN needs curl"
		code=000
		if wget -q --https-only -O "$2" --header="Accept: $3" "$1"; then code=200; fi
	else
		die "need curl or wget to download potluck"
	fi
	case "$code" in
	200) return 0 ;;
	404)
		if [ -z "$TOKEN" ]; then
			die "github.com/$REPO answered 404 with no token: it is private (set GH_TOKEN or GITHUB_TOKEN to a token that can read it) or has no release yet. Or use --from <release dir>."
		fi
		die "github.com/$REPO answered 404: the token in GH_TOKEN/GITHUB_TOKEN cannot read it (or it has no release yet)."
		;;
	401) die "github.com/$REPO answered 401: the token in GH_TOKEN/GITHUB_TOKEN is not valid." ;;
	403 | 429) die "github.com/$REPO answered $code: rate limited or refused. Set GH_TOKEN, or try again later." ;;
	000)
		if [ -z "$TOKEN" ]; then
			die "could not download $1 (no network?)"
		fi
		die "could not download $1"
		;;
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

# can_verify: whether this machine's openssl checks Ed25519 signatures: it verifies a known signature, and refuses
# that signature over another message (an openssl that always succeeds is not trusted).
can_verify() {
	command -v openssl >/dev/null 2>&1 || return 1
	say "$PROBE_MSG" >"$tmp/probe.txt"
	say "$PROBE_MSG, changed" >"$tmp/probe-bad.txt"
	ed25519_verify "$PROBE_KEY" "$tmp/probe.txt" "$PROBE_SIG" || return 1
	! ed25519_verify "$PROBE_KEY" "$tmp/probe-bad.txt" "$PROBE_SIG"
}

# check_signature DIR: 0 when manifest.json verifies with a trusted key; 2 when this machine's openssl cannot check
# it (why in $why); any other result ends the script: an empty or malformed key list, or no key that verifies.
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
	if ! can_verify; then
		why="no openssl here can check Ed25519 signatures"
		return 2
	fi
	[ -f "$1/manifest.json.sig" ] || die "the release has no signature (manifest.json.sig): not installing"
	sig=$(cat "$1/manifest.json.sig")
	for k in $keys; do
		if ed25519_verify "${k#ed25519:}" "$1/manifest.json" "$sig"; then
			return 0
		fi
	done
	die "the release's manifest does not verify with any release key: not installing"
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
	mv_version=$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([A-Za-z0-9._-]*\)".*/\1/p' "$1/manifest.json" | head -n 1)
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

# path_hint: how to put INSTALL_DIR on PATH for the shell this person runs (macOS's zsh does not read ~/.profile).
path_hint() {
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
	*[\'\"\`\$\\]*)
		say "  (this directory's name has characters a shell treats specially: add $INSTALL_DIR to PATH yourself)"
		return
		;;
	esac
	case "${SHELL:-}" in
	*/zsh) rc='~/.zshrc' ;;
	*/bash)
		if [ "$(uname -s)" != Darwin ]; then
			rc='~/.bashrc'
		elif [ -e "${HOME:-/nonexistent}/.bash_profile" ] || { [ ! -e "${HOME:-/nonexistent}/.bash_login" ] && [ ! -e "${HOME:-/nonexistent}/.profile" ]; }; then
			rc='~/.bash_profile'
		elif [ -e "${HOME:-/nonexistent}/.bash_login" ]; then
			rc='~/.bash_login' # bash reads only the first of .bash_profile, .bash_login and .profile that exists
		else
			rc='~/.profile'
		fi
		;;
	*/fish)
		say "  fish_add_path \"$dir\""
		return
		;;
	*) rc='~/.profile' ;;
	esac
	say "  echo 'export PATH=\"$dir:\$PATH\"' >> $rc && . $rc"
}

next_steps() {
	case ":${PATH:-}:" in
	*":$INSTALL_DIR:"*) ;;
	*)
		say ""
		say "$INSTALL_DIR is not on your PATH, so a new terminal will not find potluck. Add it with:"
		path_hint
		;;
	esac
	say ""
	say "Next:"
	say "  see what this machine can share:  potluck doctor"
	say "  start a pool:  potluck create <pool>      (then: potluck invite <name>)"
	say "  join a pool:   potluck join '<invite code>'"
}

main() {
	API="${POTLUCK_GITHUB_API:-https://api.github.com}"
	INSTALL_DIR="${POTLUCK_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}"
	TOKEN="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
	FROM=""
	TRUST_DIR=0
	MENUBAR=0
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
		--trust-dir)
			TRUST_DIR=1
			shift
			;;
		--menubar)
			MENUBAR=1
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

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/potluck.XXXXXX" 2>/dev/null || mktemp -d -t potluck)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	dl="$tmp/release"
	mkdir "$dl"
	src=""
	if [ -n "$FROM" ]; then
		# Each file is read from --from exactly once, into this script's own (0700) directory; every later step
		# reads only those copies, so a directory that changes underneath cannot swap what was checked.
		[ -d "$FROM" ] || die "--from $FROM is not a directory"
		src=$(cd "$FROM" && pwd -P)
		[ -e "$src/manifest.json" ] || die "$src has no manifest.json: not a potluck release directory"
		cat "$src/manifest.json" >"$dl/manifest.json" || die "could not read $src/manifest.json"
		if [ -e "$src/manifest.json.sig" ]; then
			cat "$src/manifest.json.sig" >"$dl/manifest.json.sig" || die "could not read $src/manifest.json.sig"
		fi
		v=$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([A-Za-z0-9._-]*\)".*/\1/p' "$dl/manifest.json" | head -n 1)
		[ -n "$v" ] || die "the manifest names no version"
		t="potluck_${v}_${os}_${arch}.tar.gz"
		[ -e "$src/$t" ] || die "release $v has no build for $os/$arch ($t)"
		cat "$src/$t" >"$dl/$t" || die "could not read $src/$t"
	else
		fetch "$API/repos/$REPO/releases/latest" "$tmp/latest.json" "application/vnd.github+json"
		tag=$(tr ',{}' '\n\n\n' <"$tmp/latest.json" | sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
		case "$tag" in
		potluck-?*) ;;
		*) die "the newest release of $REPO is \"$tag\", not a potluck release" ;;
		esac
		assets "$tmp/latest.json" >"$tmp/assets"
		for f in manifest.json manifest.json.sig "potluck_${tag#potluck-}_${os}_${arch}.tar.gz"; do
			url=$(awk -v f="$f" '$1 == f { print $2; exit }' "$tmp/assets")
			[ -n "$url" ] || die "release $tag has no $f (no build for $os/$arch?)"
			fetch "$url" "$dl/$f" "application/octet-stream"
		done
	fi

	# An installed potluck checks the release and installs it; nothing from the release runs first.
	installed=$(installed_potluck)
	if [ -n "$installed" ]; then
		say "checking and installing the release with the potluck already installed ($installed)"
		if ! POTLUCK_NO_AUTOSTART=1 "$installed" upgrade --release "$dl" --dir "$INSTALL_DIR"; then
			die "potluck did not install (see above); nothing was changed"
		fi
		[ "$MENUBAR" != 1 ] || install_menubar "$dl"
		next_steps
		return 0
	fi

	# A first install: this script checks the signature before anything from the release runs.
	why=""
	if check_signature "$dl"; then
		say "signature verified with a release key"
	else
		if [ -n "$FROM" ] && [ "$TRUST_DIR" != 1 ]; then
			die "cannot check the release's signature here ($why), so not installing from $FROM. Pass --trust-dir to install it unchecked, or put an OpenSSL 3 or newer first on PATH (Homebrew's openssl@3 is keg-only)."
		fi
		if [ -n "$FROM" ]; then
			say "note: the signature was NOT checked ($why); installing from the --from directory because of --trust-dir."
			say "      The new potluck will say its manifest is signed by a key it trusts: that key came in the same directory, so it adds no independent proof."
		else
			say "note: the signature was NOT checked by this script ($why); trusting the download over HTTPS from github.com."
			say "      It still checks the tarball against the release's manifest, and the new potluck then checks the manifest's"
			say "      signature against the key built into it, but that key came in the same download, so this adds no independent proof."
			case "$why" in
			*openssl*)
				say "      Once that potluck has run, running this script again as it is adds nothing. To check the signature"
				say "      before anything from the release runs: install OpenSSL 3 or newer and put its bin directory first on PATH"
				say "      (Homebrew's openssl@3 is keg-only: its bin directory is under brew --prefix openssl@3), delete $INSTALL_DIR/potluck,"
				say "      and run this script again."
				;;
			esac
		fi
	fi

	# The version and the tarball's hash come from the manifest (signed, when checked above).
	version=$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([A-Za-z0-9._-]*\)".*/\1/p' "$dl/manifest.json" | head -n 1)
	[ -n "$version" ] || die "the manifest names no version"
	grep -q '"schema":[[:space:]]*"potluck-release/1"' "$dl/manifest.json" || die "the manifest is not a potluck-release/1 manifest"
	tarball="potluck_${version}_${os}_${arch}.tar.gz"
	[ -f "$dl/$tarball" ] || die "release $version has no build for $os/$arch ($tarball)"
	want=$(awk -v f="\"$tarball\":" '$1 == f { v = $2; gsub(/[",]/, "", v); print v; exit }' "$dl/manifest.json")
	[ -n "$want" ] || die "the manifest names no $tarball"
	got=$(sha256 "$dl/$tarball")
	[ "$got" = "$want" ] || die "$tarball does not match the manifest (sha256 $got, manifest $want): not installing"
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
