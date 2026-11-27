#!/bin/bash
# A temporary keychain holding the Developer ID identity, for sign.sh on a CI Mac (docs/release-keys.md).
# Usage: macos/PotluckMenu/signing-keychain.sh open <dir>    makes <dir>, which must not exist, with the keychain in it
#        macos/PotluckMenu/signing-keychain.sh close <dir>   removes the keychain and <dir>; nothing to do is no error
#
# open reads from the environment:
#   POTLUCK_SIGN_CERT   the Developer ID Application certificate, then Apple's intermediate that issued it, as PEM
#   POTLUCK_SIGN_KEY    the certificate's private key, as unencrypted PEM
#   POTLUCK_NOTARY_KEY  optional: an App Store Connect API key's text, written to <dir>/notary.p8
# and prints one line, the identity's SHA-1 for POTLUCK_SIGN_IDENTITY. It puts the keychain first in the user's
# keychain search list (codesign builds the certificate's chain from that); close takes it out.
#
# No secret is in any program's arguments: the keychain's password, made here and kept nowhere, and every command
# that names it go to `security -i` on standard input. Secrets are on disk only in <dir>, readable only by this user;
# the private key's PEM is removed before open returns.
set -euo pipefail

fail() {
	echo "signing-keychain.sh: $*" >&2
	exit 1
}

[ $# -eq 2 ] || fail "usage: signing-keychain.sh open|close <dir>"
verb=$1 dir=$2
case $dir in /*) ;; *) fail "<dir> must be an absolute path" ;; esac
# security -i splits its lines on blanks and reads quotes and backslashes.
case $dir in *[!A-Za-z0-9._/-]*) fail "<dir> may hold only letters, digits and . _ / -" ;; esac
keychain=$dir/signing.keychain-db

close() {
	if [ -e "$keychain" ]; then
		security delete-keychain "$keychain" || rm -f "$keychain" # delete-keychain also takes it off the search list
	fi
	rm -rf "$dir"
}

case $verb in
close)
	close
	[ ! -e "$dir" ] || fail "could not remove $dir"
	exit 0
	;;
open) ;;
*) fail "usage: signing-keychain.sh open|close <dir>" ;;
esac

cert=${POTLUCK_SIGN_CERT:-} key=${POTLUCK_SIGN_KEY:-} notary=${POTLUCK_NOTARY_KEY:-}
unset POTLUCK_SIGN_CERT POTLUCK_SIGN_KEY POTLUCK_NOTARY_KEY # no program started here inherits them
[ -n "$cert" ] || fail "POTLUCK_SIGN_CERT is not set"
[ -n "$key" ] || fail "POTLUCK_SIGN_KEY is not set"
umask 077
[ ! -e "$dir" ] || fail "$dir exists"
mkdir "$dir"
opened=
trap '[ -n "$opened" ] || close' EXIT

# printf is this shell's own, so its arguments are no program's.
printf '%s\n' "$cert" >"$dir/cert.pem"
printf '%s\n' "$key" | /usr/bin/openssl rsa -out "$dir/key.pem" 2>/dev/null || fail "POTLUCK_SIGN_KEY is not an unencrypted RSA private key in PEM"
[ -z "$notary" ] || printf '%s\n' "$notary" >"$dir/notary.p8"
password=$(/usr/bin/openssl rand -hex 24)
printf '%s\n' \
	"create-keychain -p $password $keychain" \
	"set-keychain-settings -t 3600 $keychain" \
	"unlock-keychain -p $password $keychain" \
	"import $dir/key.pem -k $keychain -t priv -f openssl -T /usr/bin/codesign" \
	"import $dir/cert.pem -k $keychain -t cert -f pemseq" \
	"set-key-partition-list -S apple-tool:,apple: -s -k $password $keychain" |
	security -i >/dev/null 2>"$dir/said" || true # its exit status is the last line's; what it made is checked below
unset password key notary
rm -f "$dir/key.pem" "$dir/cert.pem"

# Read before the search list changes, so only this keychain's identities are seen.
ids=$(security find-identity -v -p codesigning "$keychain" | sed -n 's/^ *[0-9]*) \([0-9A-F]\{40\}\) "Developer ID Application: .*/\1/p')
[[ $ids =~ ^[0-9A-F]{40}$ ]] || fail "the certificate and key make no one valid Developer ID Application identity (expired, the wrong key, or the intermediate certificate is missing)"
old=()
while IFS= read -r k; do
	k=${k#*\"} k=${k%\"*}
	if [ -n "$k" ]; then old+=("$k"); fi
done < <(security list-keychains -d user)
security list-keychains -d user -s "$keychain" ${old[@]+"${old[@]}"}
opened=1
echo "$ids"
