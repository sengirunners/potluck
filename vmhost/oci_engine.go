package vmhost

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type dockerRuntime struct {
	Path    string                     `json:"path"`
	Args    []string                   `json:"runtimeArgs"`
	Type    string                     `json:"runtimeType"`
	Options map[string]json.RawMessage `json:"options"`
}
type dockerEngineOwner struct {
	pid              int
	invocation, boot string
	started          uint64
}
type dockerIdentity struct {
	endpoint string
	engineID string
	owner    dockerEngineOwner
	runtime  dockerRuntime
	kata     bool
}

var verifyDockerPeer = localDockerPeer

// Resolve the CLI's selected endpoint, then query that exact socket directly.
// A Unix pathname alone is insufficient: the peer must be this host's root
// dockerd in the same mount/cgroup namespaces, not a forwarding process.
func inspectDockerRuntime(bin, runtime string) (dockerIdentity, error) {
	return inspectPinnedDockerRuntime(bin, runtime, "")
}
func inspectPinnedDockerRuntime(bin, runtime, endpoint string) (dockerIdentity, error) {
	var id dockerIdentity
	if !OCIEngineVerificationSupported(bin) {
		return id, fmt.Errorf("live OCI runtime verification requires Docker")
	}
	if endpoint == "" {
		endpoint = os.Getenv("DOCKER_HOST")
	}
	if endpoint == "" {
		out, err := boundedHostCommand(2*time.Second, bin, "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}")
		if err != nil {
			return id, fmt.Errorf("resolve selected Docker context: %w", err)
		}
		if err := json.Unmarshal(out, &endpoint); err != nil {
			return id, fmt.Errorf("unverifiable Docker context endpoint: %w", err)
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || !filepath.IsAbs(u.Path) || u.RawQuery != "" || u.Fragment != "" {
		return id, fmt.Errorf("Docker endpoint must be a verified local Unix engine, got %q", endpoint)
	}
	var owner dockerEngineOwner
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "unix", u.Path)
		if err != nil {
			return nil, err
		}
		owner, err = verifyDockerPeer(c)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get("http://docker/info")
	if err != nil {
		return id, fmt.Errorf("verify local Docker engine: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return id, fmt.Errorf("live Docker info for runtime %s returned %s", runtime, resp.Status)
	}
	var info struct {
		ID, CgroupDriver, CgroupVersion string
		Runtimes                        map[string]dockerRuntime
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&info); err != nil {
		return id, err
	}
	if info.ID == "" {
		return id, fmt.Errorf("live Docker engine identity is missing")
	}
	r, ok := info.Runtimes[runtime]
	if !ok {
		return id, fmt.Errorf("no runtime %s has a verifiable live registration", runtime)
	}
	if r.Path == "" && r.Type == "" {
		active, err := activeDockerRuntime(owner, runtime)
		if err != nil {
			return id, err
		}
		if active.Path != "" || len(active.Args) > 0 || active.Type == "" {
			return id, fmt.Errorf("live Docker info contradicts the successful native registration snapshot")
		}
		r = active
	}
	id = dockerIdentity{endpoint: endpoint, engineID: info.ID, owner: owner, runtime: r}
	executable := r.Path
	if r.Type != "" {
		if r.Path != "" || len(r.Args) > 0 {
			return id, fmt.Errorf("ambiguous live runtime registration")
		}
		executable = r.Type
	}
	if !filepath.IsAbs(executable) {
		return id, fmt.Errorf("live Docker runtime registration omits an absolute executable/type or its options; this engine cannot establish native runtime identity")
	}
	version, err := boundedHostCommand(2*time.Second, executable, "--version")
	if err != nil {
		return id, fmt.Errorf("verify active runtime executable: %w", err)
	}
	first := strings.SplitN(strings.TrimSpace(string(version)), "\n", 2)[0]
	switch {
	case strings.HasPrefix(first, "runsc version "):
		if r.Type != "" || len(r.Options) > 0 {
			return id, fmt.Errorf("unsupported live runsc shim/options identity")
		}
	case strings.HasPrefix(first, "Kata Containers containerd shim (Rust): id: io.containerd.kata."), strings.HasPrefix(first, "containerd-shim-kata"), strings.HasPrefix(first, "kata-runtime "), strings.HasPrefix(first, "kata-runtime:"):
		id.kata = true
	default:
		return id, fmt.Errorf("active runtime executable %s is not a verified runsc or Kata identity", executable)
	}
	if id.kata && (r.Type == "" || info.CgroupDriver != "systemd" || info.CgroupVersion != "2") {
		return id, fmt.Errorf("Kata requires exposed live shim options and Docker systemd cgroup v2")
	}
	return id, nil
}

// Pinned operations ignore later context/config selection changes.
func dockerEngineEnv(endpoint string) []string {
	env := os.Environ()
	if endpoint == "" {
		return env
	}
	out := env[:0]
	for _, v := range env {
		if !strings.HasPrefix(v, "DOCKER_HOST=") && !strings.HasPrefix(v, "DOCKER_CONTEXT=") {
			out = append(out, v)
		}
	}
	return append(out, "DOCKER_HOST="+endpoint, "DOCKER_CONTEXT=")
}

// dockerAllocation is stored once with the run. Handles pin transport without
// rediscovering the engine on every output/port page. New allocations and boots
// revalidate the registration; ordinary transport uses this fixed endpoint.
type dockerAllocation struct{ Endpoint, EngineID, RegistrationSHA, Generation string }

func (id dockerIdentity) allocation() dockerAllocation {
	b, _ := json.Marshal(id.runtime)
	sum := sha256.Sum256(b)
	return dockerAllocation{id.endpoint, id.engineID, fmt.Sprintf("%x", sum), fmt.Sprintf("%s/%s/%d/%d", id.owner.boot, id.owner.invocation, id.owner.pid, id.owner.started)}
}
func (r record) bindLayer(vm vmLayer) vmLayer {
	if o, ok := vm.(ociCLI); ok && r.Engine != nil {
		if bin := r.Config["oci_cli"]; bin != "" {
			o.bin = bin
		}
		if runtime := r.Config["oci_runtime"]; runtime != "" {
			o.runtime = runtime
		}
		o.engineHost = r.Engine.Endpoint
		o.engine = r.Engine
		o.nativeBound = r.HostBudget != nil
		return o
	}
	return vm
}
func (o ociCLI) verifyEngine() (dockerIdentity, error) {
	id, err := inspectPinnedDockerRuntime(o.bin, o.runtime, o.engineHost)
	if err != nil {
		return id, err
	}
	if o.engine != nil && *o.engine != id.allocation() {
		return id, fmt.Errorf("Docker engine or active runtime generation differs from the owned allocation")
	}
	return id, nil
}

// OCIEngineVerificationSupported is the engine policy shared by readiness and allocation.
func OCIEngineVerificationSupported(cli string) bool { return filepath.Base(cli) == "docker" }
