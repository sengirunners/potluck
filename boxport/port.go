package boxport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/sengirunners/potluck/protocol"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

type Command func(stdin io.Reader, stdout, stderr io.Writer, argv ...string) *exec.Cmd

func refusal(code, msg string, args ...any) *protocol.Error {
	return &protocol.Error{Code: code, Message: fmt.Sprintf(msg, args...)}
}

var portStreamID = regexp.MustCompile(`^[0-9a-f]{48}$`)

func Exchange(ctx context.Context, a protocol.PortRequest, deadline time.Time, command Command) (protocol.PortResult, error) {
	if !time.Now().Before(deadline) {
		return protocol.PortResult{}, refusal("conflict", "sandbox lease expired")
	}
	if len(a.Data) > protocol.PortPage {
		return protocol.PortResult{}, refusal("bad_request", "port page exceeds 64 KiB")
	}
	switch a.Op {
	case "open":
		if a.Port < 1 || a.Port > 65535 || a.Stream != "" || len(a.Data) > 0 {
			return protocol.PortResult{}, refusal("bad_request", "open needs only a port in 1..65535")
		}
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return protocol.PortResult{}, err
		}
		a.Stream = hex.EncodeToString(b)
	case "read", "write", "close":
		if !portStreamID.MatchString(a.Stream) || a.Port != 0 || (a.Op != "write" && len(a.Data) > 0) {
			return protocol.PortResult{}, refusal("bad_request", "invalid port stream operation")
		}
	default:
		return protocol.PortResult{}, refusal("bad_request", "port operation is open, read, write or close")
	}
	// A busy answer means the guest took nothing (the stream's previous page is
	// still waiting for its peer to read), so the same page is sent again until
	// busyWait passes, which keeps the whole call under the daemon's verb limit.
	busyUntil := time.Now().Add(busyWait)
	var b []byte
	for {
		var err error
		if b, err = exchangeOnce(ctx, a, deadline, command); err != nil {
			return protocol.PortResult{}, err
		}
		if len(b) != 1 || b[0] != 'B' {
			break
		}
		if !time.Now().Before(busyUntil) || !time.Now().Before(deadline) {
			return protocol.PortResult{}, refusal("unavailable", "sandbox port peer is not reading")
		}
		select {
		case <-ctx.Done():
			return protocol.PortResult{}, refusal("unavailable", "sandbox port channel failed: %v", ctx.Err())
		case <-time.After(busyRetry):
		}
	}
	if b[0] == 'E' {
		return protocol.PortResult{}, refusal("unavailable", "sandbox port is unavailable")
	}
	if a.Op == "open" {
		return protocol.PortResult{Stream: a.Stream}, nil
	}
	return protocol.PortResult{Data: b[1:], EOF: b[0] == 'F'}, nil
}

const (
	busyWait  = 20 * time.Second // below the daemon's 30 s limit on a driver call
	busyRetry = 50 * time.Millisecond
)

// exchangeOnce runs the guest helper once and returns its non-empty answer.
func exchangeOnce(ctx context.Context, a protocol.PortRequest, deadline time.Time, command Command) ([]byte, error) {
	// Unix socket name includes this lease's VM identity. Streams from an old
	// stopped lease cannot be addressed after a restart with a new VM.
	out, errb := boundedBuffer{limit: protocol.PortPage + 1}, boundedBuffer{limit: 4096}
	cmd := command(bytes.NewReader(a.Data), &out, &errb, "perl", "-e", guestScript, a.Op, a.Stream, strconv.Itoa(a.Port), strconv.FormatInt(deadline.Unix(), 10))
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := runPortCommand(ctx, cmd); err != nil {
		return nil, refusal("unavailable", "sandbox port channel failed: %v", err)
	}
	b := out.Bytes()
	if out.cut || len(b) > protocol.PortPage+1 || len(b) == 0 {
		return nil, refusal("unavailable", "invalid sandbox port answer")
	}
	return b, nil
}

func runPortCommand(ctx context.Context, cmd *exec.Cmd) error {
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	defer stop()
	return cmd.Wait()
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
	cut   bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	room := max(0, b.limit-b.Len())
	if len(p) > room {
		b.cut = true
		p = p[:room]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
