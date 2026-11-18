package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/portstream"
	"github.com/sengirunners/potluck/protocol"
)

const (
	portArguments = "port refuses invalid ports, stream ids and oversized pages"
	portDuplex    = "port transfers binary data concurrently and preserves uploads after read EOF"
	portForeign   = "port stream belongs to its original box"
	portClosed    = "port close revokes the stream"
	portLease     = "port refuses an ended lease"
)

func (r *run) portCall(h string, req protocol.PortRequest) (protocol.PortResult, error) {
	raw, _ := json.Marshal(req)
	var out protocol.PortResult
	err := must(r.call(protocol.VerbPort, h, raw, &out))
	return out, err
}
func (r *run) ports() {
	if r.kind != "box" {
		return
	}
	if !r.has(protocol.VerbPort) {
		r.skip("port channels", "describe lists no port verb")
		return
	}
	h, err := r.acquireAny(r.longTTL())
	if err != nil {
		r.add(portArguments, err)
		return
	}
	defer r.endHeld(h)
	checks := []protocol.PortRequest{{Op: "open", Port: 0}, {Op: "open", Port: 65536}, {Op: "read", Stream: "../another-box"}, {Op: "write", Stream: strings.Repeat("0", 48), Data: make([]byte, protocol.PortPage+1)}}
	var invalid error
	for _, req := range checks {
		raw, _ := json.Marshal(req)
		if e := r.expect(protocol.VerbPort, h.Handle, raw, protocol.CodeBadRequest); e != nil {
			invalid = e
			break
		}
	}
	r.add(portArguments, invalid)
	if !r.has(verbSpawn) || !r.has(verbOutput) {
		r.skip(portDuplex, "probe needs spawn and output")
		return
	}
	// A real peer consumes a small seed before the concurrent exchange.
	// This checks both directions without imposing any private buffering policy
	// on independent drivers. The peer half-closes its response and still
	// expects the final upload page after read EOF.
	const probe = `use strict;use warnings;use IO::Socket::INET;$|=1;my$l=IO::Socket::INET->new(LocalAddr=>'127.0.0.1',LocalPort=>0,Listen=>1,Proto=>'tcp') or die;print $l->sockport."\n";my$c=$l->accept() or die;alarm 20;my$seed='';sysread($c,$seed,1)==1&&$seed eq'S' or die;print "seed-read\n";my$b=pack('C*',0,255,128,42)x16384;my$out=$b;while(length$out){my$n=syswrite($c,$out);die unless$n;substr($out,0,$n)=''}shutdown($c,1);my$in='';my$upload=$b.$b;while(length($in)<length($upload)){my$n=sysread($c,my$p,65536);die unless$n;$in.=$p}die 'changed upload' unless$in eq$upload;print "upload-ok\n";`

	// The probe prints its port once it listens. A cold guest can take
	// seconds to start perl, so the check waits for that line (up to
	// listenS) rather than guessing how fast the guest is; the probe's own
	// limit leaves the exchange its time after the longest such wait.
	const listenS, probeS = 30, 60
	pid, err := r.spawn(h.Handle, "argv", []string{"perl", "-e", probe}, "request_id", r.key(), "timeout_s", probeS)
	port, next := 0, int64(0)
	if err == nil {
		var out string
		out, next, err = r.waitStdout(h.Handle, pid, 0, "\n", listenS*time.Second)
		if err == nil {
			line, _, _ := strings.Cut(out, "\n")
			if port, err = strconv.Atoi(line); err != nil || port < 1 || port > 65535 {
				port, err = 0, fmt.Errorf("guest port probe printed %q, not a port", line)
			}
		} else {
			err = fmt.Errorf("guest port probe did not listen: %w", err)
		}
	}
	var stream string
	if err == nil {
		stream, err = r.portDuplexProbe(h.Handle, pid, port, next)
	}
	if err == nil {
		var ended outputResult
		ended, err = r.waitEnd(h.Handle, pid, 5*time.Second)
		if err == nil && (ended.Exit == nil || *ended.Exit != 0) {
			err = fmt.Errorf("guest rejected uploaded bytes")
		}
	}
	r.add(portDuplex, err)
	if stream != "" {
		other, e := r.acquireAny(r.longTTL())
		if e == nil {
			_, e = r.portCall(other.Handle, protocol.PortRequest{Op: "read", Stream: stream})
			r.endHeld(other)
			if e == nil {
				err = fmt.Errorf("another box accepted the stream")
			} else {
				err = nil
			}
		} else {
			err = e
		}
		r.add(portForeign, err)
		_, err = r.portCall(h.Handle, protocol.PortRequest{Op: "close", Stream: stream})
		if err == nil {
			if _, e := r.portCall(h.Handle, protocol.PortRequest{Op: "read", Stream: stream}); e == nil {
				err = fmt.Errorf("closed stream remains readable")
			}
		}
		r.add(portClosed, err)
	}
	r.endHeld(h)
	raw, _ := json.Marshal(protocol.PortRequest{Op: "open", Port: max(port, 1)})
	r.add(portLease, r.expect(protocol.VerbPort, h.Handle, raw, protocol.CodeConflict))
}

// portDuplexProbe runs the exchange with the probe listening on port; from
// is the probe's stdout offset after its port line.
func (r *run) portDuplexProbe(h, pid string, port int, from int64) (string, error) {
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	defer cancel()
	stream := ""
	call := func(ctx context.Context, req protocol.PortRequest) (protocol.PortResult, error) {
		if err := ctx.Err(); err != nil {
			return protocol.PortResult{}, err
		}
		out, err := r.portCall(h, req)
		if req.Op == "open" {
			stream = out.Stream
		}
		return out, err
	}
	conn, err := portstream.Dial(ctx, port, call)
	if err != nil {
		return stream, err
	}
	if _, err := conn.Write([]byte("S")); err != nil {
		return stream, err
	}
	if _, _, err := r.waitStdout(h, pid, from, "seed-read\n", 5*time.Second); err != nil {
		return stream, fmt.Errorf("port peer did not read seed: %w", err)
	}
	page := bytes.Repeat([]byte{0, 255, 128, 42}, protocol.PortPage/4)
	written := make(chan error, 1)
	go func() { _, err := conn.Write(page); written <- err }()
	got, readErr := io.ReadAll(io.LimitReader(conn, int64(len(page)+1)))
	writeErr := <-written
	if readErr != nil {
		return stream, readErr
	}
	if writeErr != nil {
		return stream, writeErr
	}
	if !bytes.Equal(got, page) {
		return stream, fmt.Errorf("concurrent response returned %d changed bytes", len(got))
	}
	// EOF ends only the response direction; the peer still consumes upload.
	if _, err := conn.Write(page); err != nil {
		return stream, err
	}
	return stream, nil
}
