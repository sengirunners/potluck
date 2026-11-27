package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// multicastGroup is the group the multicast check sends to: an
// organization-local one (RFC 2365) no program here is likely to use.
var multicastGroup = net.IPv4(239, 255, 77, 77)

// multicastWait is how long the check waits for a datagram after the probe
// sent its last one.
const multicastWait = 2 * time.Second

// multicast checks that what a run or box sends to a multicast group reaches
// no program on this machine that joined the group (NetMulticast). This
// machine joins the group on every interface that takes multicast (a VM's
// bridge among them: what a box sends arrives there), shows a datagram sent
// from here arrives, then has the guest send its own. A guest whose datagram
// arrives fails; one that cannot be shown to arrive from here either is a
// SKIP, as it proves nothing.
func (r *run) multicast(in func(...string) (int, error)) {
	check := "a " + r.kind + "'s multicast reaches no program on this machine"
	if r.kind == "run" {
		r.skip(check, "not proven: a run may create its network interface after the suite joins the group; use a box target")
		return
	}
	g, err := joinMulticast(multicastGroup)
	if err != nil {
		r.skip(check, "not proven: this machine cannot join a multicast group: "+err.Error())
		return
	}
	defer g.close()
	to := net.JoinHostPort(multicastGroup.String(), strconv.Itoa(g.port))
	ctl := []byte("potluck-multicast-control-" + randomTag())
	if err := sendFromHere(to, ctl); err != nil || !g.await(ctl, multicastWait) {
		why := "a datagram sent from here did not arrive"
		if err != nil {
			why = err.Error()
		}
		r.skip(check, "not proven: "+to+" does not reach a program on this machine from here either ("+why+")")
		return
	}
	tok := []byte("potluck-multicast-" + randomTag())
	code, err := in("mcast", multicastGroup.String(), strconv.Itoa(g.port), string(tok))
	switch {
	case err != nil:
	case code == exitNoTools:
		r.skip(check, "the guest has no nc")
		return
	case code == exitUsage:
		err = errors.New("the committed probe predates the multicast check: commit --print-probe again")
	case code != 0:
		err = fmt.Errorf("the probe exited %d", code)
	case g.await(tok, multicastWait):
		err = fmt.Errorf("a datagram the %s sent to %s reached a program on this machine that joined the group", r.kind, to)
	}
	r.add(check, err)
}

// mcastJoin is a multicast group joined on every interface that takes it, all on
// one port, and what has arrived.
type mcastJoin struct {
	port  int
	conns []*net.UDPConn
	mu    sync.Mutex
	got   [][]byte
	more  chan struct{}
}

// joinMulticast joins group on every up interface that takes multicast, on
// one port this machine chooses. At least one must take it.
func joinMulticast(group net.IP) (*mcastJoin, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	j := &mcastJoin{more: make(chan struct{}, 1)}
	var errs []error
	for i := range ifs {
		ifi := ifs[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s addresses: %w", ifi.Name, err))
			continue
		}
		hasIPv4 := false
		for _, addr := range addrs {
			if ip, _, err := net.ParseCIDR(addr.String()); err == nil && ip.To4() != nil {
				hasIPv4 = true
			}
		}
		if !hasIPv4 {
			continue
		}
		c, err := net.ListenMulticastUDP("udp4", &ifi, &net.UDPAddr{IP: group, Port: j.port})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ifi.Name, err))
			continue
		}
		if j.port == 0 {
			j.port = c.LocalAddr().(*net.UDPAddr).Port
		}
		j.conns = append(j.conns, c)
		go j.read(c)
	}
	if len(errs) > 0 || len(j.conns) == 0 {
		j.close()
		if len(errs) == 0 {
			errs = append(errs, errors.New("no interface takes multicast"))
		}
		return nil, errors.Join(errs...)
	}
	return j, nil
}

func (j *mcastJoin) read(c *net.UDPConn) {
	b := make([]byte, 2048)
	for {
		n, _, err := c.ReadFromUDP(b)
		if err != nil {
			return
		}
		j.mu.Lock()
		j.got = append(j.got, bytes.Clone(b[:n]))
		j.mu.Unlock()
		select {
		case j.more <- struct{}{}:
		default:
		}
	}
}

// await reports whether a datagram holding tok arrived within d.
func (j *mcastJoin) await(tok []byte, d time.Duration) bool {
	end := time.After(d)
	for {
		j.mu.Lock()
		for _, g := range j.got {
			if bytes.Contains(g, tok) {
				j.mu.Unlock()
				return true
			}
		}
		j.mu.Unlock()
		select {
		case <-j.more:
		case <-end:
			return false
		}
	}
}

func (j *mcastJoin) close() {
	for _, c := range j.conns {
		c.Close()
	}
}

// sendFromHere sends b to the multicast address to from this machine, as
// the guest's probe does from inside, three times.
func sendFromHere(to string, b []byte) error {
	c, err := net.Dial("udp4", to)
	if err != nil {
		return err
	}
	defer c.Close()
	for range 3 {
		if _, err := c.Write(b); err != nil {
			return err
		}
	}
	return nil
}
