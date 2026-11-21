package conformance

import (
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
)

// LocalPeerPort answers a NetPeerPort for a run whose guest shares this
// machine's network (a driver whose runs or boxes are processes here, such
// as a fake layer in tests): a span of peerPortSpan ports, each an nc
// listener in the probe, that nothing holds now and that no earlier call in
// this process answered.
//
// The default (47123) will not do there: it is in Linux's ephemeral range,
// so on a busy machine an outbound connection can hold one of the span, nc
// then cannot bind it and exits at once, and the probe answers 66, failing
// "the net probe sees a port open in the guest" as "cannot listen"
// (zm-4dm4, zm-e6v8). So the span is below the default ephemeral ranges
// (Linux 32768-60999, macOS 49152-65535), and each port must bind on all
// addresses as nc's listener does. Another process can still take a port
// after this answers; the random pick over some 12000 ports makes that rare.
func LocalPeerPort() (int, error) {
	const lo, hi = 20000, 32768
	givenPorts.Lock()
	defer givenPorts.Unlock()
	for range 200 {
		p := lo + rand.IntN(hi-lo-peerPortSpan)
		free := true
		for i := 0; free && i < peerPortSpan; i++ {
			l, err := net.Listen("tcp", ":"+strconv.Itoa(p+i))
			free = err == nil && !givenPorts.m[p+i]
			if err == nil {
				l.Close()
			}
		}
		if free {
			for i := range peerPortSpan {
				givenPorts.m[p+i] = true
			}
			return p, nil
		}
	}
	return 0, fmt.Errorf("no %d free ports in a row in %d-%d", peerPortSpan, lo, hi)
}

// givenPorts are the ports LocalPeerPort gave out: parallel runs in one
// process never share one.
var givenPorts = struct {
	sync.Mutex
	m map[int]bool
}{m: map[int]bool{}}
