package vmhost

// Box secrets (docs/protocol.md, Box secrets): the secrets verb puts a
// secret in a box for the box's life, not one command's. Each value is a
// 0600 file in a 0700 directory of the commands' user, guestMem/potluck-secrets,
// which the guest's mount table must show on a tmpfs or ramfs in a guest that
// swaps nowhere but zram; there is no fallback to TMPDIR or any disk, and any
// other guest (a macOS guest: no /dev/shm, no mount table) refuses. Anything
// else at the path (a link, another user's directory) is refused and never
// handed to a command. A value goes to the guest on the layer's exec channel,
// framed on stdin: never in argv, an environment, the VM's config, a record
// or a log, here or in the guest. A new value is written beside the old and
// renamed over it, so a reader never sees half a file. Every later exec,
// spawn and terminal without secrets of its own finds the directory in
// POTLUCK_SECRETS. Nothing removes it but unset: it lives in the guest's
// memory, which goes with the VM on every way a box ends (release, expiry,
// reclaim, a lost admission lease, a volume quota, the supervisor's death
// and the layer's own deadman, the sweep after a host restart), and on a
// kept disk's stop, which powers the guest off.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

const (
	// guestMem is the guest's memory filesystem and guestMounts its mount
	// table, which says whether guestMem really is one. The fake layer
	// (fake_test.go) points both at a VM directory of its own.
	guestMem    = "/dev/shm"
	guestMounts = "/proc/self/mounts"
	// guestSwaps is the guest's swap table: a tmpfs page can be swapped out,
	// so a guest swapping anywhere but zram (compressed memory) takes no
	// secrets. Without the table, /proc/meminfo beside it must say SwapTotal 0.
	guestSwaps = "/proc/swaps"
	// noMemFS and noSwap are what the guest says, and exits 125 on, when it
	// has no memory filesystem for secrets, or swaps to a disk: nothing ran.
	noMemFS = "potluck-driver-tart: this box has no memory filesystem: secrets are refused"
	noSwap  = "potluck-driver-tart: this box swaps to a disk: secrets are refused"
	// secretsLimit bounds one secrets call's guest exec.
	secretsLimit = 20 * time.Second
)

// perlMemFS is memfs(path, mounts): whether the mount table says the
// filesystem holding path (its real path, symlinks resolved) is a tmpfs or
// ramfs. The last, longest mount point holding the path decides (a later
// mount covers an earlier one). An unreadable table is no memory filesystem.
// No single quotes: execWrap carries it inside sh's.
const perlMemFS = `sub memfs { my ($p, $mt) = @_; require Cwd; my $r = Cwd::abs_path($p); return 0 unless defined $r;
	open(my $m, "<", $mt) or return 0; my ($best, $type) = (-1, "");
	while (my $l = <$m>) { my (undef, $mp, $ty) = split / /, $l; next unless defined $ty; $mp =~ s/\\([0-7]{3})/chr(oct($1))/ge;
		if (($mp eq "/" || $r eq $mp || index($r, "$mp/") == 0) && length($mp) >= $best) { ($best, $type) = (length $mp, $ty) } }
	close $m; return $type eq "tmpfs" || $type eq "ramfs" }`

// perlSwap is swapsafe(swaps): whether the guest swaps nowhere but zram. An
// unreadable table and no SwapTotal 0 in meminfo beside it is unsafe.
const perlSwap = `sub swapsafe { my ($sw) = @_;
	if (open(my $s, "<", $sw)) { my $head = <$s>; while (my $l = <$s>) { my ($dev) = split " ", $l; next unless defined $dev; return 0 unless $dev =~ m{^/dev/zram[0-9]+$} } close $s; return 1 }
	(my $mi = $sw) =~ s{[^/]*$}{meminfo}; open(my $m, "<", $mi) or return 0;
	while (my $l = <$m>) { return $1 == 0 if $l =~ /^SwapTotal:\s+([0-9]+)/ } return 0 }`

// secretsProg runs in the guest for one secrets call: $ARGV[0] is guestMem,
// $ARGV[1] guestMounts, $ARGV[2] guestSwaps; stdin is the frame
// (frameBoxSecrets). It changes nothing unless all of the call fits: 64 KiB
// held in all, names and values. It prints "<count> <dir>". Exit 2: a bad
// frame; 3: over 64 KiB; 4: no memory filesystem; 5: the directory could not
// be written; 6: the guest swaps to a disk; 7: something not a private
// directory of this user is at the directory's path. Its messages name no
// secret. Calls take turns on a lock file beside the directory (never in it:
// the directory goes when it holds no name), so an earlier call the host gave
// up on cannot rename an older value over a newer one; then a write a killed
// call left (.NAME.pid) is removed.
const secretsProg = `use strict; use warnings; use Fcntl qw(:DEFAULT :flock);
my ($mem, $mnt, $swp) = @ARGV;
sub quit { print STDERR "potluck-driver-tart: $_[1]\n"; exit $_[0] }
sub rd { my $n = shift; my $b = ""; while (length($b) < $n) { my $r = sysread(STDIN, my $c, $n - length $b); quit(2, "secrets cut short") unless $r; $b .= $c } $b }
sub ln { my $l = ""; while (1) { my $r = sysread(STDIN, my $c, 1); quit(2, "secrets cut short") unless $r; last if $c eq "\n"; $l .= $c; quit(2, "bad secrets frame") if length $l > 80 } $l }
` + perlMemFS + `
` + perlSwap + `
my $re = qr/^[A-Z][A-Z0-9_]{0,63}$/;
my $n = ln(); quit(2, "bad secrets frame") unless $n =~ /^[0-9]{1,4}$/;
my %set;
for (1 .. $n) { my ($k, $len) = split / /, ln(), 2; quit(2, "bad secrets frame") unless defined $len && $k =~ $re && $len =~ /^[0-9]{1,6}$/; $set{$k} = rd($len) }
my $u = ln(); quit(2, "bad secrets frame") unless $u =~ /^[0-9]{1,4}$/;
my @unset;
for (1 .. $u) { my $k = ln(); quit(2, "bad secrets frame") unless $k =~ $re; push @unset, $k }
my $nomem = "this box has no memory filesystem: box secrets are refused, never written to its disk";
quit(4, $nomem) unless memfs($mem, $mnt);
quit(6, "this box swaps to a disk: box secrets are refused") unless swapsafe($swp);
umask 077;
sysopen(my $lk, "$mem/.potluck-secrets.lock", O_RDWR | O_CREAT | O_NOFOLLOW, 0600) or quit(7, "the box secrets lock is not a file of this user");
my @lst = stat $lk;
quit(7, "the box secrets lock is not a file of this user") unless @lst && -f _ && $lst[4] == $<;
flock($lk, LOCK_EX) or quit(5, "cannot lock the box secrets directory");
my $dir = "$mem/potluck-secrets";
mkdir $dir, 0700 unless lstat $dir;
my @st = lstat $dir;
quit(7, "the box secrets path is not a directory of this user") unless @st && -d _ && !-l _ && $st[4] == $<;
chmod 0700, $dir or quit(5, "cannot make the box secrets directory private");
quit(4, $nomem) unless memfs($dir, $mnt);
opendir(my $th, $dir) or quit(5, "cannot read the box secrets directory");
for my $t (grep { /^\.[A-Z][A-Z0-9_]{0,63}\.[0-9]+$/ } readdir $th) { unlink "$dir/$t" }
closedir $th;
my %drop = map { $_ => 1 } @unset, keys %set;
opendir(my $dh, $dir) or quit(5, "cannot read the box secrets directory");
my $held = 0;
for my $k (grep { $_ =~ $re } readdir $dh) { next if $drop{$k}; my $s = -s "$dir/$k"; $held += length($k) + ($s || 0) }
closedir $dh;
$held += length($_) + length($set{$_}) for keys %set;
quit(3, "box secrets are over 64 KiB held (names and values)") if $held > 65536;
for my $k (@unset) { unlink "$dir/$k" or $!{ENOENT} or quit(5, "cannot remove a secret") }
for my $k (sort keys %set) {
	my $tmp = "$dir/.$k.$$";
	unlink $tmp;
	sysopen(my $f, $tmp, O_WRONLY | O_CREAT | O_EXCL, 0600) or quit(5, "cannot write a secret");
	my $v = $set{$k};
	while (length $v) { my $w = syswrite($f, $v); unless (defined $w && $w > 0) { unlink $tmp; quit(5, "cannot write a secret") } substr($v, 0, $w) = "" }
	close $f or do { unlink $tmp; quit(5, "cannot write a secret") };
	chmod 0600, $tmp;
	rename $tmp, "$dir/$k" or do { unlink $tmp; quit(5, "cannot write a secret") };
}
opendir($dh, $dir) or quit(5, "cannot read the box secrets directory");
my $c = grep { $_ =~ $re } readdir $dh;
closedir $dh;
rmdir $dir unless $c;
print "$c $dir\n";
`

// frameBoxSecrets is what secretsProg reads: exec's frame of the secrets to
// set (frameSecrets), then a count line and one line per name to unset.
func frameBoxSecrets(a protocol.BoxSecretsArgs) []byte {
	var b bytes.Buffer
	b.Write(frameSecrets(a.Secrets))
	fmt.Fprintf(&b, "%d\n", len(a.Unset))
	for _, k := range a.Unset {
		b.WriteString(k + "\n")
	}
	return b.Bytes()
}

// boxSecretsServed says whether a box of this binding takes the secrets
// verb: one whose image is a Linux guest's (a macOS guest has no memory
// filesystem the driver can prove: it refuses, and describe does not list
// the capability).
func boxSecretsServed(os string) bool { return os == "linux" }

// secretsListed refuses an exec's or spawn's secrets (n of them) in a box
// whose binding lists no secrets capability (a macOS guest): bad_request, as
// the protocol says for secrets without the capability, before anything runs.
func secretsListed(r record, n int) error {
	if n > 0 && !boxSecretsServed(r.OS) {
		return refuse("bad_request", "%s is a %s guest, which has no memory filesystem for secrets: this binding lists no secrets capability", r.Handle, r.OS)
	}
	return nil
}

// boxSecrets is the secrets verb: set and unset a box's secrets.
func (d *drv) boxSecrets(handle string, raw json.RawMessage) (any, error) {
	a, err := protocol.ParseBoxSecrets(raw)
	if err != nil {
		return nil, refuse("bad_request", "%v", err)
	}
	r, err := d.readyBox(handle)
	if err != nil {
		return nil, err
	}
	if !boxSecretsServed(r.OS) { // describe lists no box-secrets: the protocol's bad_request
		return nil, refuse("bad_request", "%s is a %s guest, which has no memory filesystem for secrets: this binding lists no box-secrets capability", r.Handle, r.OS)
	}
	var out, errb capped
	var res protocol.BoxSecretsResult
	err = flocked(filepath.Join(d.st.dir, "logs", r.ID+".secrets.lock"), func() error { // one call at a time per box: the 64 KiB check holds
		cmd := d.vm.Exec(r.VM, bytes.NewReader(frameBoxSecrets(a)), &out, &errb, "perl", "-e", secretsProg, guestMem, guestMounts, guestSwaps)
		ctx, cancel := context.WithTimeout(context.Background(), secretsLimit)
		defer cancel()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.WaitDelay = 2 * time.Second
		if err := cmd.Start(); err != nil {
			return refuse("unavailable", "secrets in %s: the guest could not be reached", r.Handle)
		}
		stop := context.AfterFunc(ctx, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
		defer stop()
		_ = cmd.Wait()
		// The guest's stderr is never quoted: an answer carries only these words.
		switch code := cmd.ProcessState.ExitCode(); {
		case ctx.Err() != nil:
			return refuse("unavailable", "secrets in %s: the guest did not answer in %s", r.Handle, secretsLimit)
		case code == 3:
			return refuse("bad_request", "%v", protocol.ErrSecretsBig)
		case code == 4:
			return refuse("forbidden", "%s has no memory filesystem: box secrets are refused, never written to its disk", r.Handle)
		case code == 6:
			return refuse("forbidden", "%s swaps to a disk: box secrets are refused, never swapped out", r.Handle)
		case code == 7:
			return refuse("forbidden", "something not a private directory of the box's user is at %s's secrets path: box secrets are refused", r.Handle)
		case code == 127:
			return refuse("internal", "the box image has no perl, which secrets needs")
		case code != 0:
			return refuse("internal", "writing the secrets of %s failed in the guest (exit %d)", r.Handle, code)
		}
		n, dir, ok := strings.Cut(strings.TrimSpace(out.String()), " ")
		c, cerr := strconv.Atoi(n)
		if !ok || cerr != nil || !strings.HasPrefix(dir, "/") {
			return refuse("internal", "the guest answered no secrets directory")
		}
		res = protocol.BoxSecretsResult{Dir: dir, Count: c}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// guestRefusal is the refusal for what execWrap said, exiting 125 before a
// command with secrets ran (nil: it said neither, so the command's own 125).
func guestRefusal(r record, stderr string) error {
	switch {
	case strings.HasPrefix(stderr, noMemFS):
		return refuse("forbidden", "%s has no memory filesystem: secrets are refused, never written to its disk", r.Handle)
	case strings.HasPrefix(stderr, noSwap):
		return refuse("forbidden", "%s swaps to a disk: secrets are refused, never swapped out", r.Handle)
	}
	return nil
}

// memCheckProg answers, in the guest, whether secrets may go there: exit 0,
// 4 (no memory filesystem) or 6 (swap on a disk). spawn asks it first, so a
// refusal is an answer, not a process that exits 125.
const memCheckProg = `use strict; use warnings;
my ($mem, $mnt, $swp) = @ARGV;
` + perlMemFS + `
` + perlSwap + `
exit 4 unless memfs($mem, $mnt);
exit 6 unless swapsafe($swp);
exit 0;
`

// secretsSafe asks the guest whether secrets may go there (memCheckProg).
func (d *drv) secretsSafe(r record) error {
	var out, errb capped
	cmd := d.vm.Exec(r.VM, nil, &out, &errb, "perl", "-e", memCheckProg, guestMem, guestMounts, guestSwaps)
	ctx, cancel := context.WithTimeout(context.Background(), secretsLimit)
	defer cancel()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return refuse("unavailable", "secrets in %s: the guest could not be reached", r.Handle)
	}
	stop := context.AfterFunc(ctx, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	defer stop()
	_ = cmd.Wait()
	switch code := cmd.ProcessState.ExitCode(); {
	case ctx.Err() != nil:
		return refuse("unavailable", "secrets in %s: the guest did not answer in %s", r.Handle, secretsLimit)
	case code == 4:
		return guestRefusal(r, noMemFS)
	case code == 6:
		return guestRefusal(r, noSwap)
	case code != 0:
		return refuse("internal", "checking %s's memory filesystem failed in the guest (exit %d)", r.Handle, code)
	}
	return nil
}
