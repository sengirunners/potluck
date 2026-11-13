package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func connect(t *testing.T, resolve Client) *sdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	st, ct := sdk.NewInMemoryTransports()
	ss, err := NewServer(resolve).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestSandboxSurfaceAndCaller(t *testing.T) {
	calls := 0
	var paths []string
	c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol)
		paths = append(paths, r.Method+" "+r.URL.Path)
		if api.GetHeader(r.Header, api.HeaderAs) != "machine:guest" || api.GetHeader(r.Header, api.HeaderProject) != "friends/private" || r.Header.Get("Authorization") != "Bearer restricted" {
			t.Errorf("caller changed: %v", r.Header)
		}
		if r.URL.Path == "/v0/run_targets" {
			api.WriteJSON(w, http.StatusOK, computeapi.RunTargetList{Targets: []computeapi.RunTargetView{{RunTarget: computeapi.RunTarget{Name: "remote", Kind: "box"}}}})
			return
		}
		if r.URL.Path == "/v0/run_targets/remote/handles/a/previews" {
			preview := computeapi.Preview{ID: "owned-preview", Port: 3000, URL: "https://preview.example.test/"}
			if r.Method == "GET" {
				api.WriteJSON(w, http.StatusOK, computeapi.PreviewList{Previews: []computeapi.Preview{preview}})
				return
			}
			if r.Method != "POST" {
				t.Errorf("unexpected preview method %s", r.Method)
			}
			var req computeapi.PreviewRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if req.Revoke != "" {
				if req.Revoke != "owned-preview" {
					t.Errorf("revoke id changed: %q", req.Revoke)
				}
				api.WriteJSON(w, http.StatusOK, computeapi.PreviewList{Previews: []computeapi.Preview{}})
				return
			}
			if !req.Public || req.Port != 3000 {
				t.Errorf("preview publish changed: %+v", req)
			}
			api.WriteJSON(w, http.StatusOK, preview)
			return
		}

		http.Error(w, `{"error":{"code":"not_found","message":"not found"}}`, http.StatusNotFound)
	}))
	c.As, c.Token, c.Project, c.ProjectFrom = "machine:guest", "restricted", "friends/private", "flag"
	cs := connect(t, func(context.Context) (*client.Client, error) { return c, nil })
	ls, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"sandbox_create":          "additive",
		"sandbox_delete":          "idempotent",
		"sandbox_exec":            "additive",
		"sandbox_list":            "read",
		"sandbox_output":          "read",
		"sandbox_preview_list":    "read",
		"sandbox_preview_publish": "additive",
		"sandbox_preview_revoke":  "idempotent",
		"sandbox_read_file":       "read",
		"sandbox_renew":           "idempotent",
		"sandbox_signal":          "idempotent",
		"sandbox_spawn":           "additive",
		"sandbox_start":           "idempotent",
		"sandbox_stop":            "idempotent",
		"sandbox_write_file":      "overwrite",
	}
	if len(ls.Tools) != len(want) {
		t.Fatalf("tools %d want exact %d", len(ls.Tools), len(want))
	}
	for _, tool := range ls.Tools {
		category, ok := want[tool.Name]
		if !ok {
			t.Errorf("unexpected tool %s", tool.Name)
			continue
		}
		delete(want, tool.Name)
		if tool.Name == "sandbox_create" {
			b, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var schema struct {
				Properties map[string]struct{ Description string }
			}
			if err := json.Unmarshal(b, &schema); err != nil {
				t.Fatal(err)
			}
			for _, template := range computeapi.BoxTemplates() {
				if !strings.Contains(schema.Properties["template"].Description, template+":") {
					t.Errorf("sandbox_create does not describe template %s", template)
				}
			}
		}
		ann := tool.Annotations
		if ann == nil || ann.ReadOnlyHint != (category == "read") || ann.IdempotentHint != (category == "idempotent") || ann.OpenWorldHint == nil || *ann.OpenWorldHint {
			t.Errorf("%s annotations: %+v category %s", tool.Name, ann, category)
			continue
		}
		if category == "read" {
			if ann.DestructiveHint != nil {
				t.Errorf("%s changed default read-only annotation: %+v", tool.Name, ann)
			}
		} else if ann.DestructiveHint == nil || *ann.DestructiveHint != (category == "overwrite") {
			t.Errorf("%s destructive annotation: %+v", tool.Name, ann)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
	banned := regexp.MustCompile(`(?i)\b(zm|box|run target|handle|bind)\b`)
	for _, tool := range ls.Tools {
		if !strings.HasPrefix(tool.Name, "sandbox_") || banned.MatchString(tool.Description) || tool.Description == "" {
			t.Errorf("tool surface: %+v", tool)
		}
	}
	if calls != 0 {
		t.Fatal("discovery contacted daemon")
	}
	r, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "sandbox_exec", Arguments: map[string]any{"handle": "box:remote/a", "argv": []string{"true"}}})
	if err != nil || !r.IsError || calls == 0 {
		t.Fatalf("scope refusal: %+v %v calls%d", r, err, calls)
	}
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"sandbox_preview_list", map[string]any{"handle": "box:remote/a"}},
		{"sandbox_preview_publish", map[string]any{"handle": "box:remote/a", "port": 3000, "public": true}},
		{"sandbox_preview_revoke", map[string]any{"handle": "box:remote/a", "preview": "owned-preview"}},
	} {
		before := calls
		result, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil || result.IsError || calls != before+2 {
			if result != nil {
				for _, content := range result.Content {
					if text, ok := content.(*sdk.TextContent); ok {
						t.Log(text.Text)
					}
				}
			}
			t.Fatalf("%s caller/route: %+v %v calls %d->%d paths%v", call.name, result, err, before, calls, paths)
		}
	}

}

func TestServeDrainsEOF(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
`)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = Serve(ctx, func(context.Context) (*client.Client, error) { t.Error("discovery resolved client"); return nil, nil }, in, &out)
	dec := json.NewDecoder(&out)
	n := 0
	for {
		var msg map[string]any
		err := dec.Decode(&msg)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if msg["jsonrpc"] != "2.0" {
			t.Fatalf("non-protocol stdout %+v", msg)
		}
		if msg["id"] != nil {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("got%d replies", n)
	}
}

func TestServeCancellation(t *testing.T) {
	in, w := io.Pipe()
	defer in.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, func(context.Context) (*client.Client, error) { return nil, nil }, in, io.Discard)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop on cancellation")
	}
}

func TestServeCancellationWithPendingCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sandbox_list","arguments":{}}}
`)
	go func() {
		done <- Serve(ctx, func(call context.Context) (*client.Client, error) {
			close(entered)
			<-call.Done()
			return nil, call.Err()
		}, input, io.Discard)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tool call did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pending EOF drain ignored cancellation")
	}
}

// The pinned SDK accepts batches for the negotiated 2024 revision and writes
// their replies as arrays. EOF must wait for every delayed call, not just init.
func TestServeDrainsBatchEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
[{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sandbox_list","arguments":{}}},{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sandbox_list","arguments":{}}}]
`)
	var out bytes.Buffer
	start := time.Now()
	_ = Serve(ctx, func(call context.Context) (*client.Client, error) {
		select {
		case <-time.After(100 * time.Millisecond):
			return nil, fmt.Errorf("finished normally")
		case <-call.Done():
			return nil, call.Err()
		}
	}, input, &out)
	ids := map[int]bool{}
	dec := json.NewDecoder(&out)
	batchReplies := 0
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var elements []json.RawMessage
		if raw[0] == '[' {
			if err := json.Unmarshal(raw, &elements); err != nil {
				t.Fatal(err)
			}
			batchReplies += len(elements)
		} else {
			elements = []json.RawMessage{raw}
		}
		for _, element := range elements {
			var msg struct {
				ID int `json:"id"`
			}
			if err := json.Unmarshal(element, &msg); err != nil {
				t.Fatal(err)
			}
			ids[msg.ID] = true
			if msg.ID == 2 || msg.ID == 3 {
				if !bytes.Contains(element, []byte("finished normally")) {
					t.Errorf("call did not finish normally: %s", element)
				}
			}
		}
	}
	if !ids[1] || !ids[2] || !ids[3] || batchReplies != 2 {
		t.Errorf("EOF lost batch replies: ids%v batchReplies%d", ids, batchReplies)
	}
	if time.Since(start) > time.Second {
		t.Error("completed batch did not release EOF drain")
	}
}

// Rejections come from the actual negotiated SDK session. They must close
// promptly rather than waiting for replies to a batch the SDK did not accept.
func TestServeRejectsInvalidBatchWithoutDrainStall(t *testing.T) {
	const call = `{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"sandbox_list","arguments":{}}}`
	for _, tt := range []struct{ name, version, frame string }{
		{"empty", "2024-11-05", `[]`},
		{"malformed element", "2024-11-05", `[` + call + `,42]`},
		{"invalid id", "2024-11-05", `[{"jsonrpc":"2.0","id":{},"method":"tools/list"}]`},
		{"duplicate ids", "2024-11-05", `[` + call + `,` + call + `]`},
		{"unsupported batch revision", "2025-06-18", `[` + call + `]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			in, w := io.Pipe()
			r, out := io.Pipe()
			defer in.Close()
			defer w.Close()
			defer r.Close()
			defer out.Close()
			done := make(chan error, 1)
			go func() {
				err := Serve(ctx, func(context.Context) (*client.Client, error) {
					t.Error("rejected batch executed tool")
					return nil, fmt.Errorf("unexpected call")
				}, in, out)
				out.Close()
				done <- err
			}()
			cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, &sdk.IOTransport{Reader: r, Writer: w}, &sdk.ClientSessionOptions{ProtocolVersion: tt.version})
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			start := time.Now()
			if _, err := io.WriteString(w, tt.frame+"\n"); err != nil {
				t.Fatal(err)
			}
			w.Close()
			select {
			case <-done:
				if time.Since(start) > time.Second {
					t.Fatal("SDK rejection stalled EOF drain")
				}
			case <-time.After(time.Second):
				t.Fatal("SDK rejection left pending drain")
			}
		})
	}
}
