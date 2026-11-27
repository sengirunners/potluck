#!/bin/sh
# Signs, and when asked notarizes, a built app (docs/menubar.md, "Signing and notarizing").
# Usage: macos/PotluckMenu/sign.sh <path to the .app>
#        macos/PotluckMenu/sign.sh --programs <dir>   signs every Mach-O file under dir (programs, not an app), no notary
#
#   POTLUCK_SIGN_IDENTITY   a Developer ID Application identity (name, or SHA-1) in the keychain.
#                           Unset (and no notary settings): the app stays as build.sh left it, signed ad hoc.
#   POTLUCK_NOTARY_PROFILE  a `xcrun notarytool store-credentials` keychain profile, or
#   POTLUCK_NOTARY_KEY, POTLUCK_NOTARY_KEY_ID, POTLUCK_NOTARY_ISSUER
#                           an App Store Connect API key (path to AuthKey_*.p8, its key id, its issuer id).
#
# Asking for a signature never falls back to ad hoc: an identity the keychain does not hold, notary settings
# without an identity, or half an API key all fail. No secret is printed.
set -eu

fail() {
	echo "sign.sh: $*" >&2
	exit 1
}

usage="usage: sign.sh <path to the .app> | sign.sh --programs <dir>"
programs=
if [ "${1:-}" = --programs ]; then
	[ $# -eq 2 ] || fail "$usage"
	programs=${2%/}
	[ -d "$programs" ] || fail "$programs is not a directory"
else
	[ $# -eq 1 ] || fail "$usage"
	app=${1%/}
	[ -f "$app/Contents/Info.plist" ] || fail "$app is not an app bundle (no Contents/Info.plist)"
fi

identity=${POTLUCK_SIGN_IDENTITY:-}
profile=${POTLUCK_NOTARY_PROFILE:-}
key=${POTLUCK_NOTARY_KEY:-}
key_id=${POTLUCK_NOTARY_KEY_ID:-}
issuer=${POTLUCK_NOTARY_ISSUER:-}

# Which notary credentials were asked for: profile, key, or none. Checked before anything is signed.
notary=none
if [ -n "$key$key_id$issuer" ]; then
	[ -n "$key" ] || fail "POTLUCK_NOTARY_KEY (path to the AuthKey .p8) is not set"
	[ -n "$key_id" ] || fail "POTLUCK_NOTARY_KEY_ID is not set"
	[ -n "$issuer" ] || fail "POTLUCK_NOTARY_ISSUER is not set"
	[ -r "$key" ] || fail "POTLUCK_NOTARY_KEY names a file that cannot be read"
	notary=key
fi
if [ -n "$profile" ]; then
	[ "$notary" = none ] || fail "set POTLUCK_NOTARY_PROFILE or the POTLUCK_NOTARY_KEY variables, not both"
	notary=profile
fi
if [ "$notary" != none ] && [ -z "$identity" ]; then
	fail "notarizing needs POTLUCK_SIGN_IDENTITY: a notary submission of an ad hoc app is always rejected"
fi

if [ -n "$programs" ]; then
	[ -n "$identity" ] || fail "--programs needs POTLUCK_SIGN_IDENTITY"
	[ "$notary" = none ] || fail "--programs does not notarize: Apple's notary service takes the app that carries them"
fi

if [ -z "$identity" ]; then
	codesign --verify --deep --strict --verbose=2 "$app" ||
		fail "the app has no valid ad hoc signature either; build it with build.sh"
	echo "sign.sh: POTLUCK_SIGN_IDENTITY is not set: $app stays signed ad hoc." \
		"It runs on this Mac only, and Gatekeeper rejects it on any other." >&2
	exit 0
fi

# "-" is codesign's ad hoc identity, and a leading dash would read as an option.
case $identity in -*) fail "POTLUCK_SIGN_IDENTITY must name a Developer ID identity, not start with '-'" ;; esac
# Fail loudly on an identity the keychain does not hold (codesign says only "no identity found").
security find-identity -v -p codesigning | grep -F -q -- "$identity" ||
	fail "no valid code signing identity matching POTLUCK_SIGN_IDENTITY is in the keychain (security find-identity -v -p codesigning)"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

sign() { # sign PATH: Apple's timestamp service is not always there, so a failure is tried twice more
	for pause in 5 20 0; do
		codesign --force --options runtime --timestamp --prefix sh.potluck. --sign "$identity" "$1" && return 0
		[ "$pause" = 0 ] || sleep "$pause"
	done
	fail "could not sign $1"
}

if [ -n "$programs" ]; then
	find "$programs" -type f -print | while IFS= read -r f; do
		case $(file -b "$f") in *Mach-O*) echo "$f" ;; esac
	done >"$tmp/order"
	[ -s "$tmp/order" ] || fail "no Mach-O file is under $programs"
	while IFS= read -r f; do
		sign "$f"
		codesign --verify --strict --verbose=2 "$f"
	done <"$tmp/order"
	echo "sign.sh: signed the programs under $programs with the identity named in POTLUCK_SIGN_IDENTITY (hardened runtime, timestamped)."
	exit 0
fi

# Nested code first, deepest first, then the app: never --deep, which signs everything alike and
# leaves a wrong seal on frameworks and helpers. Bundles are signed as bundles; loose Mach-O files
# (helpers, dylibs) one by one. The app's own executable is sealed by the app's signature.
find "$app/Contents" -mindepth 1 -type d \( -name '*.framework' -o -name '*.app' -o -name '*.xpc' -o -name '*.appex' \) \
	-print >"$tmp/found"
# A .bundle or .plugin is signed as a bundle only when it holds code; a resource-only one is sealed by the app.
find "$app/Contents" -mindepth 1 -type d \( -name '*.bundle' -o -name '*.plugin' \) -print | while IFS= read -r b; do
	find "$b" -type f -print | while IFS= read -r f; do
		case $(file -b "$f") in *Mach-O*) echo "$b"; break ;; esac
	done
done >>"$tmp/found"
main_exe="$app/Contents/MacOS/$(/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' "$app/Contents/Info.plist")"
find "$app/Contents" -type f -print | while IFS= read -r f; do
	[ "$f" = "$main_exe" ] && continue
	case $(file -b "$f") in *Mach-O*) echo "$f" ;; esac
done >>"$tmp/found"
# Depth is the count of slashes; sort deepest first.
awk '{ n = gsub("/", "/"); print n "\t" $0 }' "$tmp/found" | sort -t "$(printf '\t')" -k1,1nr -k2 |
	cut -f2- >"$tmp/order"

while IFS= read -r nested; do
	sign "$nested"
done <"$tmp/order"
sign "$app"
codesign --verify --deep --strict --verbose=2 "$app"
echo "sign.sh: signed $app with the identity named in POTLUCK_SIGN_IDENTITY (hardened runtime, timestamped)."

if [ "$notary" = none ]; then
	echo "sign.sh: no notary settings: $app is signed but not notarized, so Gatekeeper still warns on other Macs." >&2
	exit 0
fi

notarytool() { # notarytool ARGS...: the verb and its arguments, then the credentials
	if [ "$notary" = profile ]; then
		xcrun notarytool "$@" --keychain-profile "$profile"
	else
		xcrun notarytool "$@" --key "$key" --key-id "$key_id" --issuer "$issuer"
	fi
}

zip="$tmp/notarize.zip"
ditto -c -k --keepParent "$app" "$zip"
echo "sign.sh: submitting to Apple's notary service and waiting for the answer"
# The plist answer is read with plutil, not scraped. notarytool may exit non-zero for an Invalid submission
# as well as for a failure to submit, so the answer is read either way: an id means Apple has a log to fetch.
submit_ok=1
notarytool submit "$zip" --wait --timeout 30m --output-format plist >"$tmp/submit.plist" || submit_ok=0
id=$(plutil -extract id raw -o - "$tmp/submit.plist" 2>/dev/null) || id=
status=$(plutil -extract status raw -o - "$tmp/submit.plist" 2>/dev/null) || status=
if [ -z "$id" ]; then
	cat "$tmp/submit.plist" >&2
	fail "notarytool submit gave no submission id"
fi
if [ "$status" != Accepted ] && [ "$status" != Invalid ] && [ "$status" != Rejected ]; then
	fail "Apple gave no answer in time for submission $id (${status:-unknown}); run this again later"
fi
if [ "$status" != Accepted ]; then
	echo "sign.sh: notarization of submission $id ended as ${status:-unknown}; Apple's log:" >&2
	notarytool log "$id" >&2 || true
	fail "notarization rejected"
fi
[ "$submit_ok" = 1 ] || fail "notarytool submit failed after accepting the submission"
xcrun stapler staple "$app"
xcrun stapler validate "$app"
spctl --assess -t exec -vv "$app"
echo "sign.sh: notarized and stapled $app; Gatekeeper accepts it."
