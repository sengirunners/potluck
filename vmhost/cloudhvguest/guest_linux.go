//go:build linux

package cloudhvguest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// vsockConn is one accepted vsock connection. Close shuts the socket down
// first, so a reader blocked on it returns.
type vsockConn struct {
	fd int
	f  *os.File
}

func (c vsockConn) Read(p []byte) (int, error)  { return c.f.Read(p) }
func (c vsockConn) Write(p []byte) (int, error) { return c.f.Write(p) }
func (c vsockConn) Close() error {
	_ = unix.Shutdown(c.fd, unix.SHUT_RDWR)
	return c.f.Close()
}

// Run is potluck-guest: it sets the guest's address from the VM's vsock
// context id, then answers the host on Port for as long as the guest runs.
// Commands run as boxUser, an ordinary account of the image's: the agent
// itself is root (it sets the network, the clock and the caches), and a
// command is not. The account has sudo only in a VM whose binding has
// root=on, which the host says with each exec (Request.Root, giveSudo): the
// image has no rule of its own, and one left from an earlier boot is removed
// here. A guest without that account runs no command at all. dns are the
// resolvers written to /etc/resolv.conf (none: left alone).
func Run(boxUser string, dns []string, log func(string, ...any)) error {
	command, err := asUser(boxUser)
	if err != nil {
		return err
	}
	if err := dropSudo(sudoRule); err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("vsock: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: Port}); err != nil {
		return fmt.Errorf("vsock bind: %w", err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		return fmt.Errorf("vsock listen: %w", err)
	}
	go func() { // the host can run commands before the network is up
		if err := network(dns); err != nil {
			log("network: %v", err)
		}
	}()
	h := Hooks{Squeeze: squeeze, Woke: woke, Command: command, Root: once(func() error { return giveSudo(sudoRule, boxUser) }), Docker: once(startDocker)}
	for {
		nfd, peer, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("vsock accept: %w", err)
		}
		// Only the host: a guest whose kernel loops vsock back would let its
		// own user reach the agent.
		if vm, ok := peer.(*unix.SockaddrVM); !ok || vm.CID != unix.VMADDR_CID_HOST {
			_ = unix.Close(nfd)
			continue
		}
		go Serve(vsockConn{nfd, os.NewFile(uintptr(nfd), "vsock")}, h)
	}
}

// once answers a hook that runs f until it has succeeded, one call at a time,
// and nothing after that.
func once(f func() error) func() error {
	var mu sync.Mutex
	done := false
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return nil
		}
		err := f()
		done = err == nil
		return err
	}
}

// sudoRule is the sudoers file that gives the box's user root. Only the
// agent writes it (giveSudo), so a box has sudo because its host said so in
// this boot, never because of what is on its disk.
const sudoRule = "/etc/sudoers.d/potluck-box"

// sudoName is an account name that means only itself in a sudoers file.
var sudoName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// dropSudo removes the sudo rule at path that an earlier boot's agent wrote,
// if there is one.
func dropSudo(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("an earlier boot's sudo rule could not be removed: %w", err)
	}
	return nil
}

// giveSudo writes the sudo rule at path that lets name run anything as root
// with no password. The rule is written whole under another name and renamed
// in, since sudo reads the directory at every call, and the temporary name
// has a dot, which sudo skips.
func giveSudo(path, name string) error {
	if !sudoName.MatchString(name) {
		return fmt.Errorf("the account %q is not a name a sudo rule can carry", name)
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("this image has no sudo: root=on needs one that has it, as scripts/cloudhv-image.sh builds")
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(name+" ALL=(ALL:ALL) NOPASSWD: ALL\n"), 0o440); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dockerUnit is the image's Docker, a systemd unit that nothing starts at
// boot (scripts/cloudhv-image.sh).
const dockerUnit = "docker.service"

// dockerStartFor bounds one try at starting Docker.
const dockerStartFor = 60 * time.Second

// startDocker has systemd start dockerUnit and waits until Docker says it is
// up (the unit is Type=notify), so the command this comes before, and a boot
// with it, finds Docker answering. It fails in an image without the unit.
func startDocker() error {
	ctx, cancel := context.WithTimeout(context.Background(), dockerStartFor)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "systemctl", "start", dockerUnit).CombinedOutput(); err != nil {
		return fmt.Errorf("this image's Docker (%s) did not start: docker=on needs an image that has it, as scripts/cloudhv-image.sh builds: %v: %s",
			dockerUnit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// asUser answers the Hooks.Command that runs a command as name: its ids and
// groups, its home as the directory, and a login's environment.
func asUser(name string) (func(*exec.Cmd), error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("this image has no user %q for commands to run as: %v", name, err)
	}
	uid, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil || uid == 0 {
		return nil, fmt.Errorf("user %q cannot run commands: it must be an ordinary account, not root", name)
	}
	groups := []uint32{uint32(gid)}
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil && uint32(n) != uint32(gid) {
				groups = append(groups, uint32(n))
			}
		}
	}
	env := []string{"HOME=" + u.HomeDir, "USER=" + name, "LOGNAME=" + name, "SHELL=/bin/sh",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	return func(cmd *exec.Cmd) {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}
		cmd.Env, cmd.Dir = env, u.HomeDir
	}, nil
}

// localCID is this VM's vsock context id.
func localCID() (uint32, error) {
	f, err := os.Open("/dev/vsock")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return unix.IoctlGetUint32(int(f.Fd()), unix.IOCTL_VM_SOCKETS_GET_LOCAL_CID)
}

// network gives the guest's one NIC the address its context id names, a
// default route through the host's end of the link, and the resolvers.
func network(dns []string) error {
	cid, err := localCID()
	if err != nil {
		return err
	}
	ip, gw, ok := GuestAddr(cid)
	if !ok {
		return fmt.Errorf("context id %d names no address: the network is left as the image has it", cid)
	}
	var nic string
	for deadline := time.Now().Add(20 * time.Second); nic == "" && time.Now().Before(deadline); {
		ifs, _ := net.Interfaces()
		for _, i := range ifs {
			if i.Flags&net.FlagLoopback == 0 && len(i.HardwareAddr) == 6 {
				nic = i.Name
				break
			}
		}
		if nic == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if nic == "" {
		return fmt.Errorf("no network card")
	}
	for _, a := range [][]string{
		{"link", "set", "dev", nic, "up"},
		{"addr", "replace", ip.String() + "/30", "dev", nic},
		{"route", "replace", "default", "via", gw.String(), "dev", nic},
	} {
		if out, err := exec.Command("ip", a...).CombinedOutput(); err != nil {
			return fmt.Errorf("ip %s: %v: %s", strings.Join(a, " "), err, strings.TrimSpace(string(out)))
		}
	}
	if len(dns) > 0 {
		var b strings.Builder
		for _, d := range dns {
			b.WriteString("nameserver " + d + "\n")
		}
		_ = os.Remove("/etc/resolv.conf") // a link to a stub resolver that is not running
		if err := os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// squeeze hands back what the guest is not using: file cache is dropped and
// free memory compacted, so free page reporting returns whole blocks to the
// host. Running programs keep their memory.
func squeeze() error {
	unix.Sync()
	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0o200); err != nil {
		return err
	}
	_ = os.WriteFile("/proc/sys/vm/compact_memory", []byte("1\n"), 0o200) // absent on a kernel built without compaction
	return nil
}

// woke runs right after a restore: the guest's clock stood still while it
// was napping, and its random pool is the one it went to sleep with.
func woke(unixNano int64) error {
	if unixNano > 0 {
		ts := unix.NsecToTimespec(unixNano)
		if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
			return fmt.Errorf("set the clock: %w", err)
		}
	}
	if f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0); err == nil {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(unixNano))
		_, _ = f.Write(b[:])
		_ = unix.IoctlSetInt(int(f.Fd()), unix.RNDRESEEDCRNG, 0)
		f.Close()
	}
	return nil
}
