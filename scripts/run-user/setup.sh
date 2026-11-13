#!/bin/sh
# One-time Mac setup for a dedicated, non-admin account that holds what the owner's other programs must not
# reach (the runner App key) and runs VMs on a volume of its own.
# The Mac's owner runs it with sudo. Running it again changes nothing that is already right, and finishes a
# run that was cut short.
#
#   sudo scripts/run-user/setup.sh --owner <you> [--user zmrun] [--volume zmrun] [--quota 200g] [--lock-owner-home]
#   sudo scripts/run-user/setup.sh --undo [--user zmrun]
#
# Setup makes:
#   - a hidden user and group (default zmrun): not an admin, no password, no login shell;
#   - its home /Users/<user>, mode 0700, on the Data volume (FileVault covers it: secrets go here);
#   - an APFS volume (default zmrun) in the Data volume's container, owned by the user, mode 0700, for VMs
#     (TART_HOME=/Volumes/<volume>/tart; ~/.tart links to it), left out of Spotlight and Time Machine. It shares
#     the container's free space; --quota caps it. FileVault does not cover it, so nothing secret goes on it.
# --lock-owner-home makes the owner's home 0700. macOS makes homes 0755, so without it the user can read
# whatever the owner's home lets everyone read. Like every local user, it is in the staff group.
#
# /var/db/potluck-run-user.<user> records what setup made (owner, volume UUID, the home mode it changed), and
# --undo removes exactly that: the volume (and every VM on it), the user, its home and group, and the owner's
# home mode if it is still 0700. It refuses while the user has processes running.
set -eu

RUN_USER=zmrun VOLUME= OWNER= QUOTA= LOCK= UNDO=
while [ $# -gt 0 ]; do
	case $1 in
	--owner) OWNER=${2-}; shift 2 ;;
	--user) RUN_USER=${2-}; shift 2 ;;
	--volume) VOLUME=${2-}; shift 2 ;;
	--quota) QUOTA=${2-}; shift 2 ;;
	--lock-owner-home) LOCK=1; shift ;;
	--undo) UNDO=1; shift ;;
	*) echo "setup: unknown argument $1" >&2; exit 2 ;;
	esac
done
die() { echo "setup: $*" >&2; exit 1; }
say() { echo "setup: $*"; }
plain_name() {
	case $1 in "" | [!a-z_]* | *[!a-z0-9_-]*) return 1 ;; esac
	[ ${#1} -le 32 ]
}
[ "$(uname)" = Darwin ] || die "macOS only"
[ "$(id -u)" = 0 ] || die "run it with sudo"
plain_name "$RUN_USER" || die "--user is not a user name"
[ -z "$VOLUME" ] || plain_name "$VOLUME" || die "--volume must be a plain name like zmrun"
case $QUOTA in "" | [1-9]*[mgt]) ;; *) die "--quota is a size like 200g" ;; esac
case ${QUOTA%?} in *[!0-9]*) die "--quota is a size like 200g" ;; esac
ROOT=${POTLUCK_SETUP_ROOT-} # tests only: a directory standing in for /
RUN_HOME=$ROOT/Users/$RUN_USER
STATE=$ROOT/var/db/potluck-run-user.$RUN_USER # root's: key value lines
MARK=potluck-run-user                         # the Comment on the user and group this script makes

state() { [ ! -f "$STATE" ] || awk -v k="$1" '$1 == k {v = $2} END {print v}' "$STATE"; } # the last one wins
remember() { (umask 077 && printf '%s %s\n' "$1" "$2" >>"$STATE"); }
# plist_get <key>: one value from `diskutil info -plist` on stdin.
plist_get() { plutil -extract "$1" raw -o - - 2>/dev/null; }
attr() { dscl . -read "$1" "$2" 2>/dev/null | awk 'NR == 1 {print $2}'; }
user_exists() { dscl . -read "/Users/$RUN_USER" >/dev/null 2>&1; }
group_exists() { dscl . -read "/Groups/$RUN_USER" >/dev/null 2>&1; }
data_container() { diskutil info -plist /System/Volumes/Data | plist_get APFSContainerReference; }
# ours_or_die <what to do>: the user and group named RUN_USER, if there, carry this script's mark.
ours_or_die() {
	rhome=$(attr "/Users/$RUN_USER" NFSHomeDirectory)
	if user_exists && { [ "$(attr "/Users/$RUN_USER" Comment)" != "$MARK" ] ||
		{ [ "$rhome" != "$RUN_HOME" ] && ! { [ -z "$rhome" ] && [ -f "$STATE" ]; }; }; }; then
		die "user $RUN_USER was not made by this script; $1"
	fi
	if group_exists && [ "$(attr "/Groups/$RUN_USER" Comment)" != "$MARK" ]; then
		die "group $RUN_USER was not made by this script; $1"
	fi
}
# our_volume: `diskutil info -plist` of the volume setup made (by its recorded UUID), or nothing.
our_volume() {
	uuid=$(state volume_uuid)
	[ -n "$uuid" ] || return 0
	info=$(diskutil info -plist "$uuid" 2>/dev/null) || return 0
	[ "$(printf '%s\n' "$info" | plist_get VolumeUUID)" = "$uuid" ] || return 0
	[ "$(printf '%s\n' "$info" | plist_get APFSContainerReference)" = "$(data_container)" ] ||
		die "volume $uuid is no longer in the Data volume's container; left alone"
	printf '%s\n' "$info"
}

if [ -n "$UNDO" ]; then
	[ -z "$OWNER$QUOTA$LOCK$VOLUME" ] || die "--undo takes only --user (the volume is the one setup recorded)"
	owner=$(state owner)
	[ "$owner" != "$RUN_USER" ] || die "$STATE names $RUN_USER as the owner; left alone"
	ours_or_die "left alone"
	if user_exists; then
		uid=$(attr "/Users/$RUN_USER" UniqueID)
		[ -z "$uid" ] || ! ps -U "$uid" -o pid= | grep -q . ||
			die "$RUN_USER has processes running; stop them first (ps -U $uid)"
	fi
	info=$(our_volume)
	if [ -n "$info" ]; then
		diskutil apfs deleteVolume "$(printf '%s\n' "$info" | plist_get DeviceIdentifier)" >/dev/null
		say "deleted volume $(printf '%s\n' "$info" | plist_get VolumeName)"
	fi
	if user_exists; then
		dscl . -delete "/Users/$RUN_USER"
		say "deleted user $RUN_USER"
	fi
	if group_exists; then
		dscl . -delete "/Groups/$RUN_USER"
		say "deleted group $RUN_USER"
	fi
	if [ -f "$STATE" ] && { [ -e "$RUN_HOME" ] || [ -L "$RUN_HOME" ]; }; then
		rm -rf "$RUN_HOME"
		say "deleted $RUN_HOME"
	fi
	mode=$(state owner_home_mode)
	home=$(attr "/Users/$owner" NFSHomeDirectory)
	if [ -n "$mode" ] && [ -n "$home" ] && [ -d "$home" ] && [ "$(stat -f %Lp "$home")" = 700 ]; then
		case $mode in [0-7][0-7][0-7]) chmod "$mode" "$home"; say "$home is mode $mode again" ;; esac
	fi
	[ -f "$STATE" ] || say "no record of a setup for $RUN_USER"
	rm -f "$STATE"
	say "undone"
	exit 0
fi

[ -n "$OWNER" ] || die "--owner is required (the Mac user who runs Potluck)"
[ "$RUN_USER" != "$OWNER" ] || die "the run user must not be the owner"
OWNER_HOME=$(attr "/Users/$OWNER" NFSHomeDirectory)
[ -n "$OWNER_HOME" ] && [ -d "$OWNER_HOME" ] || die "--owner names no user with a home"
if [ -f "$STATE" ]; then
	was=$(state owner)
	[ "$was" = "$OWNER" ] || die "$RUN_USER was set up for $was; run --undo first"
	had=$(state volume)
	[ -z "$VOLUME" ] || [ "$VOLUME" = "$had" ] || die "$RUN_USER was set up with volume $had; run --undo first"
	VOLUME=$had
else
	# A first run takes over nothing: a user, group, home or volume already there is someone else's.
	VOLUME=${VOLUME:-zmrun}
	! user_exists || die "user $RUN_USER already exists; pick another --user"
	! group_exists || die "group $RUN_USER already exists; pick another --user"
	[ ! -e "$RUN_HOME" ] && [ ! -L "$RUN_HOME" ] || die "$RUN_HOME already exists; pick another --user"
	! diskutil info -plist "$VOLUME" >/dev/null 2>&1 || die "a volume $VOLUME already exists; pick another --volume"
	[ ! -e "$ROOT/Volumes/$VOLUME" ] && [ ! -L "$ROOT/Volumes/$VOLUME" ] ||
		die "$ROOT/Volumes/$VOLUME already exists; pick another --volume"
	mkdir -p "$(dirname "$STATE")"
	remember owner "$OWNER"
	remember volume "$VOLUME"
fi
MNT=$ROOT/Volumes/$VOLUME
ours_or_die "run --undo first"

# An id free as both a user and a group, from 600 (below 500 is the system's).
free_id() {
	used=$( (dscl . -list /Users UniqueID; dscl . -list /Groups PrimaryGroupID) | awk '{print $2}')
	n=600
	while printf '%s\n' "$used" | grep -qx "$n"; do n=$((n + 1)); done
	echo "$n"
}

# Every attribute is set on every run, so a run cut short is finished by the next.
group_exists || say "made group $RUN_USER"
gid=$(attr "/Groups/$RUN_USER" PrimaryGroupID)
[ -n "$gid" ] || gid=$(free_id)
dscl . -create "/Groups/$RUN_USER" Comment "$MARK" # first: a record is marked from its first attribute
dscl . -create "/Groups/$RUN_USER" PrimaryGroupID "$gid"
dscl . -create "/Groups/$RUN_USER" RealName "Potluck run user"
dscl . -create "/Groups/$RUN_USER" Password "*"
user_exists || say "made user $RUN_USER"
uid=$(attr "/Users/$RUN_USER" UniqueID)
if [ -z "$uid" ]; then
	uid=$gid # the same number as its group when no user has it
	dscl . -list /Users UniqueID | awk '{print $2}' | grep -qx "$uid" && uid=$(free_id)
fi
dscl . -create "/Users/$RUN_USER" Comment "$MARK"
dscl . -create "/Users/$RUN_USER" UniqueID "$uid"
dscl . -create "/Users/$RUN_USER" PrimaryGroupID "$gid"
dscl . -create "/Users/$RUN_USER" UserShell /usr/bin/false
dscl . -create "/Users/$RUN_USER" NFSHomeDirectory "$RUN_HOME"
dscl . -create "/Users/$RUN_USER" RealName "Potluck run user"
dscl . -create "/Users/$RUN_USER" Password "*"
dscl . -create "/Users/$RUN_USER" IsHidden 1
if dseditgroup -o checkmember -m "$RUN_USER" admin >/dev/null 2>&1; then
	die "$RUN_USER is an admin: remove it (dseditgroup -o edit -d $RUN_USER -t user admin) and run again"
fi
[ ! -L "$RUN_HOME" ] || die "$RUN_HOME is a link; run --undo first"
mkdir -p "$RUN_HOME"
chown "$RUN_USER:$RUN_USER" "$RUN_HOME"
chmod 700 "$RUN_HOME"

info=$(our_volume)
if [ -z "$info" ]; then
	! diskutil info -plist "$VOLUME" >/dev/null 2>&1 || die "a volume $VOLUME exists that setup did not record; run --undo first"
	container=$(data_container)
	[ -n "$container" ] || die "cannot find the Data volume's APFS container"
	diskutil apfs addVolume "$container" APFS "$VOLUME" ${QUOTA:+-quota "$QUOTA"} >/dev/null
	info=$(diskutil info -plist "$VOLUME")
	uuid=$(printf '%s\n' "$info" | plist_get VolumeUUID)
	[ -n "$uuid" ] || die "made volume $VOLUME but cannot read its UUID; delete it with diskutil apfs deleteVolume $VOLUME"
	remember volume_uuid "$uuid"
	say "made volume $VOLUME in $container"
fi
mp=$(printf '%s\n' "$info" | plist_get MountPoint || true)
if [ -z "$mp" ]; then
	diskutil mount "$(state volume_uuid)" >/dev/null
	info=$(our_volume)
	mp=$(printf '%s\n' "$info" | plist_get MountPoint || true)
fi
[ "$mp" = "$MNT" ] || die "volume $VOLUME is mounted at $mp, not $MNT; unmount whatever holds $MNT and run again"
diskutil enableOwnership "$MNT" >/dev/null
chown "$RUN_USER:$RUN_USER" "$MNT"
chmod 700 "$MNT"
mdutil -i off "$MNT" >/dev/null 2>&1 || say "could not turn Spotlight off on $MNT"
tmutil addexclusion -v "$MNT" >/dev/null 2>&1 || say "could not leave $MNT out of Time Machine"
# The user owns $MNT and $RUN_HOME and may have put links in them: what goes inside is made as the user.
sudo -u "$RUN_USER" mkdir -p "$MNT/tart"
sudo -u "$RUN_USER" ln -sfn "$MNT/tart" "$RUN_HOME/.tart"
[ "$(readlink "$RUN_HOME/.tart")" = "$MNT/tart" ] || die "$RUN_HOME/.tart is not a link to $MNT/tart; remove it and run again"

if [ -n "$LOCK" ] && [ "$(stat -f %Lp "$OWNER_HOME")" != 700 ]; then
	[ -n "$(state owner_home_mode)" ] || remember owner_home_mode "$(stat -f %Lp "$OWNER_HOME")"
	chmod 700 "$OWNER_HOME"
	say "$OWNER_HOME is mode 700"
fi
if [ "$(stat -f %Lp "$OWNER_HOME")" != 700 ]; then
	echo "setup: WARNING: $RUN_USER can read what $OWNER_HOME lets everyone read; run again with --lock-owner-home" >&2
fi
cat <<DONE
setup: done. $RUN_USER (uid $uid, not an admin), home $RUN_HOME, VMs in $MNT/tart.
setup: undo with: sudo $0 --undo --user $RUN_USER
DONE
