package hostprobe

import (
	"context"
	"strings"
	"testing"
)

// The ordinary share entry point must derive Docker readiness from the
// explicitly requested runtime, without changing doctor's default preference.
func TestShareDockerExplicitKata(t *testing.T) {
	f := linux("docker", "runsc")
	f.paths["/dev/kvm"] = true
	r := Detect(context.Background(), &fakeRunner{out: map[string]Result{
		"docker --version":                        ok("Docker version 29.1.3"),
		"docker info --format {{json .Runtimes}}": ok(`{"runsc":{"path":"/usr/bin/runsc"},"kata-qemu":{"runtimeType":"/opt/kata/runtime-rs/bin/containerd-shim-kata-v2"}}`),
	}}, f, "")
	l, _ := r.Layer("oci")
	if l.Runtime != "runsc" {
		t.Fatalf("default preference changed: %+v", l)
	}
	o := ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "test-kata",
		Sizes: []string{"c1m2"}, DryRun: true, Docker: true, OCIRuntime: "kata-qemu"}
	res, err := Check(context.Background(), &fakeRunner{}, o)
	if err != nil {
		t.Fatalf("explicit registered Kata Docker: %v", err)
	}
	if got := res.Sizes[0].Config; got["oci_runtime"] != "kata-qemu" || got["docker"] != "on" {
		t.Fatalf("wrong runtime bound: %v", got)
	}
	if steps := strings.Join(o.RootSteps(), "\n"); strings.Contains(steps, "runsc-root") {
		t.Fatalf("Kata plan installs unrelated gVisor sudo runtime: %s", steps)
	}
	l, _ = r.Layer("oci")
	if l.Runtime != "runsc" || l.Sudo != "" {
		t.Fatalf("share changed stored default readiness: %+v", l)
	}
}

func TestShareDockerExplicitKataRefusals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registered bool
		kvm        bool
	}{
		{"unregistered", false, true},
		{"KVM inaccessible", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := bigReport()
			r.KVM = tc.kvm
			r.KVMProblem = "test KVM is not accessible"
			r.Layers[0].Sudo = SudoRuntime
			if tc.registered {
				r.Layers[0].Runtimes = append(r.Layers[0].Runtimes, "kata-qemu")
			}
			_, err := Check(context.Background(), &fakeRunner{}, ShareOptions{
				Report: r, Images: []string{pinA}, NamePrefix: "test-kata",
				Sizes: []string{"c1m2"}, DryRun: true, Docker: true, OCIRuntime: "kata-qemu"})
			if err == nil {
				t.Fatal("unusable selected runtime accepted")
			}
		})
	}
}
