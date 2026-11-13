package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// drainWait bounds how long Serve keeps answering after its input closed.
var drainWait = 30 * time.Second

// pending counts requests read but not yet answered, so that when the input
// closes (`printf ... | potluck mcp`) Serve answers what it already read before
// it stops, instead of dropping the replies.
type pending struct {
	mu   sync.Mutex
	n    int
	zero *sync.Cond
}

func newPending() *pending {
	p := &pending{}
	p.zero = sync.NewCond(&p.mu)
	return p
}

func (p *pending) add(d int) {
	p.mu.Lock()
	p.n += d
	if p.n <= 0 {
		p.n = 0
		p.zero.Broadcast()
	}
	p.mu.Unlock()
}

// wait returns once nothing is pending or after d.
func (p *pending) wait(ctx context.Context, d time.Duration) {
	stop := context.AfterFunc(ctx, func() { p.mu.Lock(); p.n = 0; p.zero.Broadcast(); p.mu.Unlock() })
	defer stop()
	t := time.AfterFunc(d, func() { p.mu.Lock(); p.n = 0; p.zero.Broadcast(); p.mu.Unlock() })
	defer t.Stop()
	p.mu.Lock()
	for p.n > 0 {
		p.zero.Wait()
	}
	p.mu.Unlock()
}

// count counts calls or replies in newline-delimited SDK messages, including
// batch arrays. Decode through the SDK so rejected elements cannot leave a
// partially counted batch waiting for replies that will never be written.
func count(b []byte, request bool) int {
	n := 0
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var elements []json.RawMessage
		if err := json.Unmarshal(line, &elements); err != nil {
			elements = []json.RawMessage{line}
		}
		batchCount := 0
		valid := true
		seen := map[jsonrpc.ID]bool{}
		for _, element := range elements {
			msg, err := jsonrpc.DecodeMessage(element)
			if err != nil {
				valid = false
				break
			}
			switch m := msg.(type) {
			case *jsonrpc.Request:
				if !m.ID.IsValid() {
					continue
				}
				if seen[m.ID] {
					valid = false
					break
				}
				seen[m.ID] = true
				if request {
					batchCount++
				}
			case *jsonrpc.Response:
				if !request {
					batchCount++
				}
			}
			if !valid {
				break
			}
		}
		if valid {
			n += batchCount
		}
	}
	return n
}

// drainReader counts each complete request line it passes on; at EOF it
// waits until they are answered.
type drainReader struct {
	ctx  context.Context
	r    io.Reader
	p    *pending
	part []byte
}

func (d *drainReader) Read(b []byte) (int, error) {
	n, err := d.r.Read(b)
	if n > 0 {
		buf := append(d.part, b[:n]...)
		i := bytes.LastIndexByte(buf, '\n')
		if i >= 0 {
			d.p.add(count(buf[:i], true))
			d.part = append([]byte(nil), buf[i+1:]...)
		} else {
			d.part = buf
		}
	}
	if err == io.EOF {
		if len(d.part) > 0 {
			d.p.add(count(d.part, true))
			d.part = nil
		}
		if n == 0 {
			d.p.wait(d.ctx, drainWait)
		}
	}
	return n, err
}

// drainWriter counts the responses written.
type drainWriter struct {
	w io.Writer
	p *pending
}

func (d drainWriter) Write(b []byte) (int, error) {
	n, err := d.w.Write(b)
	d.p.add(-count(b, false))
	return n, err
}

func (drainWriter) Close() error { return nil }

// Close releases a blocked input read when the session is cancelled.
func (d *drainReader) Close() error {
	if closer, ok := d.r.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
