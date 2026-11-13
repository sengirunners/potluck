package core

import "net/netip"

// AddrBucket is the key a client address is limited by: the exact address
// for IPv4, the /64 for IPv6 (one host holds a whole /64, so a per-address
// limit on single addresses is no limit at all). Anything that is not an
// address is its own bucket.
func AddrBucket(addr string) string {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	a = a.Unmap().WithZone("")
	if a.Is4() {
		return a.String()
	}
	p, err := a.Prefix(64)
	if err != nil {
		return addr
	}
	return p.String()
}
