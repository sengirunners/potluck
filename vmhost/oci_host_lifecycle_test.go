package vmhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the supervisor's existing admission boundary and native APIs with
// fake executables. No VM, privilege grant, or real systemd call is involved.
func TestHostBudgetAdmissionParentAndCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux Kata producer")
	}
	for _, mode := range []string{"normal", "invalid-parent", "boot-fails", "cleanup-fails", "docker-hangs", "password-sudo"} {
		t.Run(mode, func(t *testing.T) {
			profile, profileValue := testHostProfile(t)
			root := t.TempDir()
			old := hostCgroupRoot
			hostCgroupRoot = root
			t.Cleanup(func() { hostCgroupRoot = old })
			o, dir := newFakeOCI(t, "none")
			o.runtime = "kata-qemu"
			installProfileOnFakeEngine(t, profileValue, dir)
			bin := filepath.Dir(o.bin)
			docker := strings.Replace(fakeOCI, "info) if", `info) if [ "$3" = "{{.CgroupDriver}} {{.CgroupVersion}}" ]; then echo 'systemd 2'; exit 0; fi; if`, 1)
			if mode == "boot-fails" {
				docker = strings.Replace(docker, "start) n=$3;", "start) exit 7; n=$3;", 1)
			}
			if mode == "docker-hangs" {
				docker = strings.Replace(docker, "rm) n=$3;", "rm) sleep 30; n=$3;", 1)
			}
			if err := os.WriteFile(o.bin, []byte(docker), 0700); err != nil {
				t.Fatal(err)
			}
			native := `#!/bin/sh
if [ "$2" = -k ]; then shift; fi
case "$2 $3 $4" in "busctl --version "|"sh -c :") [ "$HOST_TEST_MODE" != password-sudo ] || { echo 'sudo: a password is required' >&2; exit 1; }; exit 0;; esac # verifyHostRoot
case "$2" in
busctl)
 case "$7" in
 StartTransientUnit)
  cg="$HOST_TEST_ROOT/potluck.slice/$9"; mkdir -p "$cg"
  printf '%s\n' "$HOST_TEST_MEMORY" > "$cg/memory.max"
  printf '%s 100000\n' "$HOST_TEST_QUOTA" > "$cg/cpu.max"
  printf 'populated 0\n' > "$cg/cgroup.events";;
 StopUnit) :;;
 *) exit 4;;
 esac;;
sh) if [ "$HOST_TEST_MODE" != cleanup-fails ]; then printf 'populated 0\n' > "$HOST_TEST_CG/cgroup.events"; fi;;
*) exit 3;;
esac
`
			for _, name := range []string{"sudo", "busctl"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(native), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			t.Setenv("HOST_TEST_ROOT", root)
			t.Setenv("HOST_TEST_MEMORY", "2684354560")
			t.Setenv("HOST_TEST_QUOTA", "200000")
			t.Setenv("HOST_TEST_MODE", mode)
			if mode == "invalid-parent" {
				t.Setenv("HOST_TEST_QUOTA", "max")
			}
			var deleted atomic.Int32
			chargedC := make(chan hostBudget, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "DELETE" {
					deleted.Add(1)
					w.WriteHeader(204)
					return
				}
				var req struct {
					CPU    int `json:"vcpu"`
					Memory int `json:"memory_mib"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				chargedC <- hostBudget{req.CPU, req.Memory}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": "own", "expires_at": time.Now().Add(time.Minute)})
			}))
			defer srv.Close()
			c := config{VM: "oci", OCICLI: "docker", OCIRuntime: "kata-qemu", OCIHostProfile: profile, CPU: 1, Memory: 2048, StateDir: root, AdmissionURL: srv.URL, AdmissionTTL: 120, BootTimeout: 1}
			s := sup{cfg: c, vm: o, log: io.Discard}
			rec := record{VM: "owned-vm", ID: "r000000000001", Target: "bounded", HostBudget: &hostBudget{2, 2560}}
			t.Setenv("HOST_TEST_CG", filepath.Join(root, "potluck.slice", hostParentName(root, rec.VM, rec.Starts)))
			err := s.takeRoom(rec)
			if mode == "password-sudo" { // the native parent's calls would fail: refused before any lease, naming why
				if !errors.Is(err, ErrOCIHostNeedsSudo) || len(chargedC) > 0 {
					t.Fatalf("password-only sudo: %v (lease requests %d)", err, len(chargedC))
				}
				return
			}
			var charged hostBudget
			select {
			case charged = <-chargedC:
			default:
				t.Fatalf("no external lease request: %v", err)
			}
			if charged != (hostBudget{2, 2560}) {
				t.Fatalf("external charge %+v, want total host budget", charged)
			}
			if mode == "invalid-parent" {
				if err == nil {
					t.Fatal("unbounded native parent reached boot")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.releaseRoom()
			cg := s.host.parent.path
			t.Setenv("HOST_TEST_CG", cg)
			o = s.vm.(ociCLI)
			if err := o.Clone("img", "owned-vm", 1, 2048); err != nil {
				t.Fatal(err)
			}
			args, _ := os.ReadFile(filepath.Join(dir, "c", "owned-vm", "args"))
			for _, want := range []string{"--cpus\n1\n", "--memory\n2048m\n", "--cgroup-parent\n" + s.host.parent.name + "\n"} {
				if !strings.Contains(string(args), want) {
					t.Fatalf("workload/native allocation mismatch %q: %s", want, args)
				}
			}
			if err := os.WriteFile(filepath.Join(cg, "cgroup.events"), []byte("populated 1\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "boot-fails" {
				cmd, hold, err := o.Boot("owned-vm", true, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				select {
				case err := <-done:
					if err == nil || cmd.ProcessState.ExitCode() != 7 {
						t.Fatalf("boot failure status lost: %v (%v)", err, cmd.ProcessState)
					}
				case <-time.After(3 * time.Second):
					_ = hold.Close()
					t.Fatal("attach boot failure was not observed promptly")
				}

				_ = hold.Close()
			}
			start := time.Now()
			o.Destroy("owned-vm", 0)
			if time.Since(start) > 13*time.Second {
				t.Fatal("cleanup not bounded despite Docker failure")
			}
			if mode == "cleanup-fails" {
				s.releaseRoom()
				s.adm = nil
				if deleted.Load() != 0 {
					t.Fatal("populated parent released admission")
				}
				return
			}
			if !s.host.parent.empty() {
				t.Fatal("owned parent remained populated")
			}
			s.releaseRoom()
			s.adm = nil
			if deleted.Load() != 1 {
				t.Fatalf("empty parent release count %d", deleted.Load())
			}
		})
	}
}

func TestAdmissionFailedRenewDoesNotPulseHostGuard(t *testing.T) {
	pulses := 0
	var success atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !success.Load() {
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(w, `{"expires_at":%q}`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
	}))
	defer srv.Close()
	a, err := newAdmission(config{AdmissionURL: srv.URL, AdmissionTTL: 3})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	h := held{a: a, lease: admissionLease{Lease: "own", ExpiresAt: now.Add(time.Minute)}, renewed: now.Add(-time.Minute), tried: now.Add(-time.Minute), confirmed: func(time.Time) error { pulses++; return nil }}
	if err := h.keep(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if pulses != 0 {
		t.Fatal("failed renewal extended guard")
	}
	success.Store(true)
	if err := h.keep(context.Background(), now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if pulses != 1 {
		t.Fatal("confirmed renewal did not reach guard")
	}
}
