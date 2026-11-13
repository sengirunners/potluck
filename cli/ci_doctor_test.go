package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/sengirunners/potluck/kit/api"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/runners"
)

func TestCIDoctorFailureExitAndJSON(t *testing.T) {
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	for _, mod := range m.dm.Modules() {
		if s, ok := mod.(*runners.Service); ok {
			s.DoctorRead = func(context.Context, string, any) error { return fmt.Errorf("not signed in") }
		}
	}
	for _, jsonMode := range []bool{false, true} {
		args := []string{"ci", "doctor", "acme/web"}
		if jsonMode {
			args = append(args, "--json")
		}
		out, errout, code := m.run(t, args...)
		if code == 0 {
			t.Fatalf("doctor failure exited zero: %s %s", out, errout)
		}
		if jsonMode {
			var report struct {
				Reports []runners.DoctorReport `json:"reports"`
			}
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Reports) != 1 || !report.Reports[0].Failed() {
				t.Fatalf("failure not encoded: %s", out)
			}
		} else if !strings.Contains(out, "fail") {
			t.Fatalf("missing failure text: %s", out)
		}
	}
}

func TestCIDoctorOldDaemonForkDiagnosis(t *testing.T) {
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv(EnvHome, home)
	t.Setenv("HOME", home)
	ln, err := net.Listen("unix", filepath.Join(home, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.WireProtocol)
		http.NotFound(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	for _, jsonMode := range []bool{false, true} {
		args := []string{"ci", "doctor", "acme/fork"}
		if jsonMode {
			args = append(args, "--json")
		}
		var out, errout bytes.Buffer
		code := Run(context.Background(), args, strings.NewReader(""), &out, &errout)
		if code == 0 || !strings.Contains(out.String(), "fork") || !strings.Contains(out.String(), "restart") {
			t.Fatalf("exit %d: %s %s", code, &out, &errout)
		}
		if jsonMode {
			var r struct{ Reports []runners.DoctorReport }
			if err := json.Unmarshal(out.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			if len(r.Reports) != 1 || r.Reports[0].Checks[0].Number != 6 || !r.Reports[0].Failed() {
				t.Fatalf("%+v", r)
			}
		}
	}
}

// --json keeps the daemon's whole answer: the repositories its gh budget
// did not reach included.
func TestCIDoctorJSONKeepsSkipped(t *testing.T) {
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv(EnvHome, home)
	t.Setenv("HOME", home)
	ln, err := net.Listen("unix", filepath.Join(home, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.WireProtocol)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"reports":[],"warnings":["budget"],"skipped":["acme/late"]}`)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	var out, errout bytes.Buffer
	if code := Run(context.Background(), []string{"ci", "doctor", "--json"}, strings.NewReader(""), &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s %s", code, &out, &errout)
	}
	var r runners.DoctorResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Skipped) != 1 || r.Skipped[0] != "acme/late" {
		t.Fatalf("skipped lost: %s", &out)
	}
}
