package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func TestFollowDrainsTerminalOutputAndReportsCaptureLoss(t *testing.T) {
	for _, dropped := range []int64{0, 123} {
		t.Run(fmt.Sprintf("dropped_%d", dropped), func(t *testing.T) {
			data := strings.Repeat("agent output\n", 100000)
			calls := 0
			c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				var req computeapi.OutputRunTargetRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				calls++
				start := req.StdoutFrom - dropped
				if start < 0 {
					start = 0
				}
				end := min(start+int64(req.MaxBytes), int64(len(data)))
				exit := 0
				p := computeapi.RunTargetOutput{Stdout: data[start:end], StdoutNext: end + dropped, Exit: &exit}
				if calls == 1 {
					p.DroppedStdout = dropped
				}
				api.WriteJSON(w, 200, p)
			}))
			path := filepath.Join(t.TempDir(), "stdout")
			capture, err := openTaskOutput(path)
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			opts := boxJobOptions{out: io.MultiWriter(capture, io.Discard), errw: &stderr, stdoutDropped: func(n int64) { capture.dropped += n }}
			result, err := follow(context.Background(), c, "box", "handle", opts, time.Now().Add(time.Minute), jobResult{PID: "pid"})
			if err != nil || result.State != "exited" {
				t.Fatalf("follow: %+v %v", result, err)
			}
			err = capture.close()
			if (err != nil) != (dropped > 0) {
				t.Fatalf("capture lost %d: %v", dropped, err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data || calls < 3 {
				t.Fatalf("capture has %d/%d bytes in %d calls: %v", len(got), len(data), calls, err)
			}
		})
	}
}
