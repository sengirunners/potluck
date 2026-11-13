package api

import (
	"net/http"
	"strings"
)

// The rename from zm to Potluck (zm-eakh.12.42.5) goes in steps so no
// machine needs a simultaneous upgrade: step 1 made every reader accept the
// new name beside the old one; step 2 (this build) sends and creates the new
// names, and still reads the old ones. A step 2 build talks to step 1 builds
// and to its own; it assumes no machine runs a build from before step 1.

// ProtocolNext is the daemon protocol's new name, "potluck/1". A reader takes
// it as the same major as Protocol (SameProtocol).
const ProtocolNext = "potluck/1"

// WireProtocol is what a daemon and its clients send in Potluck-Protocol and
// report in /v0/health: "potluck/1".
const WireProtocol = ProtocolNext

// AdapterProtocol is zm's work-adapter protocol, "zm/v0" (an adapter's "zm"
// field; zm's adapter.SameMajor). It does not follow the daemon's rename:
// Protocol keeps this value for importers that compare adapters with it, and
// zm moves those comparisons to AdapterProtocol.
const AdapterProtocol = "zm/v0"

// SameProtocol reports whether two protocol values have one major, taking
// "potluck/1" and "zm/v0" as one. ProtocolMajor itself is unchanged (zm's
// work adapters compare with it and keep meaning "zm/v0" only).
func SameProtocol(a, b string) bool {
	return protocolFamily(a) == protocolFamily(b)
}

func protocolFamily(p string) string {
	m := ProtocolMajor(p)
	if m == ProtocolMajor(ProtocolNext) {
		return ProtocolMajor(Protocol)
	}
	return m
}

// Header name prefixes: the old one (still read) and the new one (sent).
const (
	HeaderPrefixOld = "Zm-"
	HeaderPrefixNew = "Potluck-"
)

// HeaderNew is a Zm-* header's new name ("Zm-As" -> "Potluck-As"); any other
// name is returned as it is.
func HeaderNew(name string) string {
	if rest, ok := strings.CutPrefix(http.CanonicalHeaderKey(name), HeaderPrefixOld); ok {
		return HeaderPrefixNew + rest
	}
	return name
}

// GetHeader reads a Zm-* header under its new name first, then its old one:
// the first that is set and not empty.
func GetHeader(h http.Header, name string) string {
	if n := HeaderNew(name); n != name {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return h.Get(name)
}

// HeaderValues is every value of a Zm-* header under the name GetHeader
// reads: the new name when it has a value, else the old one.
func HeaderValues(h http.Header, name string) []string {
	if n := HeaderNew(name); n != name && h.Get(n) != "" {
		return h.Values(n)
	}
	return h.Values(name)
}

// SetHeader sets a Zm-* header under its new name, Potluck-* (what this
// build sends), and deletes it under the old one, so no reader sees a value
// the caller's request carried and this sender replaced. Any other name is
// set as it is.
func SetHeader(h http.Header, name, value string) {
	DelHeader(h, name)
	h.Set(HeaderNew(name), value)
}

// DelHeader deletes a Zm-* header under both names.
func DelHeader(h http.Header, name string) {
	h.Del(name)
	if n := HeaderNew(name); n != name {
		h.Del(n)
	}
}

// IsOwnHeader reports whether a header name is one of ours: Zm-* or
// Potluck-* (any case).
func IsOwnHeader(name string) bool {
	l := strings.ToLower(name)
	return strings.HasPrefix(l, "zm-") || strings.HasPrefix(l, "potluck-")
}

// SendOldNames renames every Potluck-* header in h to its Zm-* name; when
// both are set the Zm-* value stays. Step 1's tunnel transport called it
// before signing; this build sends the new names and no longer does.
func SendOldNames(h http.Header) {
	for k, v := range h {
		rest, ok := strings.CutPrefix(http.CanonicalHeaderKey(k), HeaderPrefixNew)
		if !ok {
			continue
		}
		delete(h, k)
		if old := HeaderPrefixOld + rest; len(h.Values(old)) == 0 {
			h[old] = v
		}
	}
}

// Tailnet node names of daemons: NodePrefixNew + machine is the name a
// daemon gives a new node; NodePrefix + machine is an existing node's (made
// before the rename's step 2), which keeps its name. Both are read. Join
// codes start with the node's name.
const (
	NodePrefix    = "zm-"
	NodePrefixNew = "potluck-"
)

// NodePrefixes are the prefixes of a daemon's node name, the new first.
var NodePrefixes = []string{NodePrefixNew, NodePrefix}

// NodeMachine splits a daemon's node name (any case) into its prefix and
// machine part: "zm-mini" and "potluck-mini" are both "mini". ok is false
// for any other name; the machine part is not validated.
func NodeMachine(name string) (prefix, machine string, ok bool) {
	l := strings.ToLower(strings.TrimSpace(name))
	for _, p := range NodePrefixes {
		if m, found := strings.CutPrefix(l, p); found {
			return p, m, true
		}
	}
	return "", "", false
}

// IsDaemonNode reports whether a tailnet node name (any case) is a daemon's
// by its name: zm-* or potluck-*. A check that refuses daemon nodes (zm's
// approvals: never pair a machine whose agents could answer) refuses both.
func IsDaemonNode(name string) bool {
	_, _, ok := NodeMachine(name)
	return ok
}
