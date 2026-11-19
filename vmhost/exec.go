package vmhost

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/boxproc"
)

// SPEC 20's exec limits.
const (
	execCap        = 1 << 20 // bytes kept of stdout and of stderr each
	execDefaultS   = 30
	execMaxS       = 600
	execHostMargin = 15 * time.Second // past timeout_s before the host gives up on the guest
)

// execWrap runs in the guest: it resolves cwd, sets HOME and TMPDIR, and
// runs argv in its own process group under a timeout. Perl (perl-base on
// Debian and Ubuntu, /usr/bin/perl on macOS) does the process group, the
// clock and the output, since a POSIX sh cannot make a group without a tty.
// The command writes into pipes Perl owns and relays; when the command exits
// (or on timeout, exit 124) Perl kills its group and stops relaying within a
// second, so nothing it left behind (a setsid'd daemon included) holds the
// answer up. $1 is the default cwd ("" = HOME), $2 the cwd asked for, $3 the
// timeout in seconds, $4 whether secrets are framed ahead of stdin, $5 a
// file name: when set (a spawned process, procs.go), the command's process
// group id is written to $TMPDIR/$5 while it runs, for signal; $6 the guest's
// memory filesystem (guestMem), $7 its mount table (guestMounts), $8 its swap
// table (guestSwaps). Secrets go only to a directory the mount table shows on
// a tmpfs or ramfs in a guest with no swap but zram, never to TMPDIR or any
// disk: another guest refuses them (noMemFS or noSwap, exit 125, before the
// command runs). Without secrets of its own, a command sees the box's
// secrets directory (secrets.go) in POTLUCK_SECRETS and ZM_SECRETS when it
// is a directory (not a link) of the user the command runs as.
const execWrap = `base=$1; cwd=$2; t=$3; sec=$4; pf=$5; mem=$6; mnt=$7; swp=$8; shift 8
` + guestHome + `TMPDIR=${TMPDIR:-/tmp}; export TMPDIR
pf=${pf:+$TMPDIR/$pf}
` + baseInHome + `[ -n "$base" ] || base=$HOME
case $cwd in "") d=$base ;; /*) d=$cwd ;; *) d=$base/$cwd ;; esac
cd "$d" 2>/dev/null || { echo "potluck-driver-tart: no directory $d in the box" >&2; exit 125; }
command -v perl >/dev/null 2>&1 || { echo "potluck-driver-tart: the box image has no perl, which exec needs" >&2; exit 125; }
exec perl -e '
use strict; use warnings; use POSIX (); use IO::Select; use Time::HiRes qw(time); use Fcntl; use File::Temp ();
my $t = shift @ARGV;
my $sec = shift @ARGV;
my $pf = shift @ARGV;
my $mem = shift @ARGV;
my $mnt = shift @ARGV;
my $swp = shift @ARGV;
my ($dir, $pid, @written);
END { if (defined $dir) { unlink map { "$dir/$_" } @written; rmdir $dir } unlink $pf if defined $pf && $pf ne "" }
$SIG{$_} = sub { kill "KILL", -$pid if $pid; exit 143 } for qw(TERM HUP INT);
delete $ENV{ZM_SECRETS}; delete $ENV{POTLUCK_SECRETS};
` + perlMemFS + `
` + perlSwap + `
my $box = "$mem/potluck-secrets";
my @bs = lstat $box; # a directory, never a link, of this user: else not given to the command
if ($sec) {
	sub rd { my $n = shift; my $b = ""; while (length($b) < $n) { my $r = sysread(STDIN, my $c, $n - length $b); die "potluck-driver-tart: secrets cut short\n" unless $r; $b .= $c } $b }
	sub ln { my $l = ""; while (1) { my $r = sysread(STDIN, my $c, 1); die "potluck-driver-tart: secrets cut short\n" unless $r; last if $c eq "\n"; $l .= $c; die "potluck-driver-tart: bad secrets frame\n" if length $l > 80 } $l }
	sub nomem { print STDERR "` + noMemFS + `\n"; exit 125 }
	nomem() unless memfs($mem, $mnt);
	unless (swapsafe($swp)) { print STDERR "` + noSwap + `\n"; exit 125 }
	my $n = ln();
	die "potluck-driver-tart: bad secrets frame\n" unless $n =~ /^[0-9]{1,4}$/;
	$dir = File::Temp::tempdir("potlucksec.XXXXXXXXXX", DIR => $mem);
	chmod 0700, $dir;
	nomem() unless memfs($dir, $mnt);
	for (1 .. $n) {
		my ($name, $len) = split / /, ln(), 2;
		die "potluck-driver-tart: bad secrets frame\n" unless defined $len && $name =~ /^[A-Z][A-Z0-9_]{0,63}$/ && $len =~ /^[0-9]{1,6}$/;
		my $v = rd($len);
		sysopen(my $f, "$dir/$name", O_WRONLY | O_CREAT | O_EXCL, 0600) or die "potluck-driver-tart: cannot write a secret\n";
		push @written, $name;
		print $f $v; close $f or die "potluck-driver-tart: cannot write a secret\n";
	}
	$ENV{ZM_SECRETS} = $dir; $ENV{POTLUCK_SECRETS} = $dir;
} elsif (@bs && POSIX::S_ISDIR($bs[2]) && $bs[4] == $<) {
	$ENV{ZM_SECRETS} = $box; $ENV{POTLUCK_SECRETS} = $box;
}
pipe(my $or, my $ow) or die "pipe: $!"; pipe(my $er, my $ew) or die "pipe: $!";
$pid = fork;
die "fork: $!" unless defined $pid;
if (!$pid) {
	close $or; close $er;
	open(STDOUT, ">&", $ow) or POSIX::_exit(126); open(STDERR, ">&", $ew) or POSIX::_exit(126);
	close $ow; close $ew; setpgrp(0, 0);
	no warnings "exec";
	exec { $ARGV[0] } @ARGV or do { print STDERR "$ARGV[0]: $!\n"; POSIX::_exit(127) };
}
close $ow; close $ew; setpgrp($pid, $pid);
if ($pf ne "") { if (open(my $f, ">", "$pf.$$")) { print $f "$pid\n"; close $f; rename "$pf.$$", $pf } }
my %to = (fileno($or) => \*STDOUT, fileno($er) => \*STDERR);
my $sel = IO::Select->new($or, $er);
my $deadline = time + $t;
my ($done, $late, $status, $grace) = (0, 0, 0, 0);
sub put { my ($fh, $b) = @_; while (length $b) { my $n = syswrite($fh, $b); last unless defined $n; substr($b, 0, $n) = "" } }
while (1) {
	if ($sel->count) {
		for my $fh ($sel->can_read(0.1)) {
			my $n = sysread($fh, my $buf, 65536);
			if (!$n) { $sel->remove($fh); next }
			put($to{fileno $fh}, $buf);
		}
	} else {
		select(undef, undef, undef, 0.1); # the command closed its output and runs on: the clock still holds
	}
	if (!$done && waitpid($pid, POSIX::WNOHANG()) == $pid) { $status = $?; $done = 1; kill "KILL", -$pid; $grace = time + 1 }
	if (!$done && time >= $deadline) { $late = 1; kill "KILL", -$pid; waitpid($pid, 0); $status = $?; $done = 1; $grace = time + 1 }
	last if $done && (!$sel->count || time >= $grace);
}
exit 124 if $late;
exit($status & 127 ? 128 + ($status & 127) : $status >> 8);
' "$t" "$sec" "$pf" "$mem" "$mnt" "$swp" "$@"`

// capped keeps the first max bytes written to it (execCap when max is 0)
// and says whether it cut. With stop, a write past max fails, so nothing
// more is copied (the process itself is the caller's to end).
type capped struct {
	buf  bytes.Buffer
	cut  bool
	max  int
	stop bool
}

// errCapped is a stop capped's answer once it cut.
var errCapped = errors.New("output over its cap")

func (c *capped) Write(p []byte) (int, error) {
	if room := cmp.Or(c.max, execCap) - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		c.cut = true
		if c.stop {
			return max(room, 0), errCapped
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capped) String() string { return strings.ToValidUTF8(c.buf.String(), "�") }

// cmdArgs are exec's args, which spawn shares.
type cmdArgs struct {
	argv     []string
	stdin    []byte
	timeout  *int
	cwd      string
	secrets  json.RawMessage
	encoding string
}

// parseCmd reads and checks exec's (and spawn's) command: a literal argv,
// stdin (base64 with encoding), cwd.
func parseCmd(raw json.RawMessage) (cmdArgs, error) {
	var a struct {
		Argv     json.RawMessage `json:"argv"`
		Stdin    *string         `json:"stdin"`
		Timeout  *int            `json:"timeout_s"`
		Cwd      string          `json:"cwd"`
		Secrets  json.RawMessage `json:"secrets"`
		Encoding string          `json:"encoding"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return cmdArgs{}, refuse("bad_request", "bad args: %v", err)
	}
	c := cmdArgs{timeout: a.Timeout, cwd: a.Cwd, secrets: a.Secrets, encoding: a.Encoding}
	if t := bytes.TrimSpace(a.Argv); len(t) == 0 || t[0] != '[' || json.Unmarshal(t, &c.argv) != nil || len(c.argv) == 0 {
		return c, refuse("bad_request", "argv is a non-empty array of strings, never a shell string")
	}
	if strings.ContainsRune(a.Cwd, 0) || slices.ContainsFunc(c.argv, func(s string) bool { return strings.ContainsRune(s, 0) }) {
		return c, refuse("bad_request", "argv or cwd holds a NUL")
	}
	var err error
	if c.stdin, err = boxproc.Decode(a.Stdin, a.Encoding); err != nil {
		return c, asRefusal(err)
	}
	return c, nil
}

// asRefusal turns boxproc's refusals into this driver's.
func asRefusal(err error) error {
	var e *boxproc.Refusal
	if errors.As(err, &e) {
		return refuse(e.Code, "%s", e.Msg)
	}
	return err
}

// readyBox loads a box that takes commands now: SPEC 20's conflict otherwise.
func (d *drv) readyBox(handle string) (record, error) {
	r, err := d.find(handle)
	if err != nil {
		return r, err
	}
	if r.State != stReady || time.Until(r.Deadline) <= 0 {
		return r, refuse("conflict", "%s is %s, not ready", r.Handle, r.State)
	}
	return r, nil
}

// wrapped is the guest argv that runs argv through execWrap: cwd resolved
// against the checkout (else HOME), the timeout, secrets framed ahead of
// stdin (sec), the group file pf ("" for exec), and the guest's memory
// filesystem, where secrets go and nowhere else (a kept disk's box too).
func wrapped(r record, cwd string, timeout int, sec bool, pf string, argv []string) []string {
	base, flag := "", "0"
	if r.Commit != "" {
		base = srcDir(r)
	}
	if sec {
		flag = "1"
	}
	return databaseWrapped(r, append([]string{"sh", "-c", execWrap, "sh", base, cwd, strconv.Itoa(timeout), flag, pf, guestMem, guestMounts, guestSwaps}, argv...))
}

// exec runs one command in a ready box (SPEC 20 "Boxes", exec).
func (d *drv) exec(handle string, raw json.RawMessage) (any, error) {
	a, err := parseCmd(raw)
	if err != nil {
		return nil, err
	}
	timeout := execDefaultS
	if a.timeout != nil {
		timeout = *a.timeout
	}
	if timeout <= 0 || timeout > execMaxS {
		return nil, refuse("bad_request", "timeout_s %d is outside 1..%d", timeout, execMaxS)
	}
	r, err := d.readyBox(handle)
	if err != nil {
		return nil, err
	}
	timeout = max(1, min(timeout, int(time.Until(r.Deadline)/time.Second))) // cut to expires_at
	secrets, err := parseSecrets(a.secrets)                                 // after the conflict check (SPEC 20)
	if err != nil {
		return nil, err
	}
	if err := secretsListed(r, len(secrets)); err != nil {
		return nil, err
	}
	var stdin io.Reader = bytes.NewReader(a.stdin)
	if len(secrets) > 0 { // framed ahead of the command's stdin: never argv or env
		stdin = io.MultiReader(bytes.NewReader(frameSecrets(secrets)), stdin)
	}
	var out, errb capped
	cmd := d.vm.Exec(r.VM, stdin, &out, &errb, wrapped(r, a.cwd, timeout, len(secrets) > 0, "", a.argv)...)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second+execHostMargin)
	defer cancel()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second // the answer never waits on a pipe something else holds
	if err := cmd.Start(); err != nil {
		return nil, refuse("unavailable", "exec in %s: %v", r.Handle, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	defer stop()
	_ = cmd.Wait()
	ended := time.Now()
	code := cmd.ProcessState.ExitCode()
	if ctx.Err() != nil {
		code = 124 // the guest did not end it in time: the host did (a backstop; the guest's own clock ends it first)
	}
	if code < 0 {
		code = 128 + int(syscall.SIGKILL)
	}
	if len(secrets) > 0 && code == 125 { // nothing ran
		if err := guestRefusal(r, errb.String()); err != nil {
			return nil, err
		}
	}
	// The box may have ended under the command. Expired, and the command
	// ended after expires_at: it was cut there (124). Released, reclaimed or
	// lost: there is no answer to give. A command that ended in time keeps its
	// own exit, whatever happened to the box after.
	if now, err := d.st.load(r.ID); err == nil && now.State != stReady {
		switch {
		case now.State == stExpired && ended.After(now.Deadline):
			code = 124
		case now.State != stExpired && !now.End.IsZero() && ended.After(now.End):
			return nil, refuse("conflict", "%s was %s while the command ran", r.Handle, now.State)
		}
	}
	res := map[string]any{"exit": code, "stdout": out.String(), "stderr": errb.String(), "truncated": out.cut || errb.cut}
	if a.encoding == boxproc.Base64 {
		res["stdout"], res["stderr"], res["encoding"] = boxproc.Encode(out.buf.Bytes(), a.encoding), boxproc.Encode(errb.buf.Bytes(), a.encoding), a.encoding
	}
	return res, nil
}

var secretNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`) // protocol.SecretName

// secretsMax bounds a command's secrets: names and values together.
const secretsMax = 64 << 10

// parseSecrets reads exec's secrets (SPEC 20 "Boxes", Secrets). Refusals name
// neither a value nor a name, and never quote the field.
func parseSecrets(raw json.RawMessage) (map[string]string, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return nil, nil
	}
	var m map[string]string
	if t[0] != '{' || json.Unmarshal(t, &m) != nil {
		return nil, refuse("bad_request", "secrets is an object of names to string values")
	}
	total := 0
	for k, v := range m {
		if !secretNameRe.MatchString(k) {
			return nil, refuse("bad_request", "a secret's name is not [A-Z][A-Z0-9_]{0,63}")
		}
		total += len(k) + len(v)
	}
	if total > secretsMax {
		return nil, refuse("bad_request", "secrets are over 64 KiB (names and values)")
	}
	return m, nil
}

// frameSecrets is what the guest wrapper reads ahead of the command's stdin:
// a count line, then per secret a "NAME LEN" line and LEN bytes.
func frameSecrets(m map[string]string) []byte {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	slices.Sort(names)
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d\n", len(names))
	for _, k := range names {
		fmt.Fprintf(&b, "%s %d\n", k, len(m[k]))
		b.WriteString(m[k])
	}
	return b.Bytes()
}
