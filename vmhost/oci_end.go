package vmhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ociEndWait bounds the inspect EndOf makes: a wedged engine delays a lost
// box's record by no more than this.
const ociEndWait = 3 * time.Second

// EndOf reads box vm's state from the container CLI (docker, podman and
// nerdctl all answer inspect with State.Running, State.OOMKilled,
// State.ExitCode and HostConfig.Memory) and, for a box under an owned host
// parent (Kata, a bound runsc host), that cgroup's memory.events, which
// outlives the container until Destroy: an oom_kill there is a kill for
// memory even when the runtime did not record one. Nothing here needs root.
func (o ociCLI) EndOf(vm string) (vmEnd, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ociEndWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, o.bin, "container", "inspect", vm)
	cmd.Env = dockerEngineEnv(o.engineHost)
	out, err := cmd.Output() // stdout only (boundedCommand mixes in stderr, where engines print warnings)
	if err != nil {
		return vmEnd{}, fmt.Errorf("%s container inspect %s: %v", o.bin, vm, err)
	}
	var cs []struct {
		State struct {
			Running   bool
			OOMKilled bool
			ExitCode  int
		}
		HostConfig struct{ Memory int64 }
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &cs); err != nil || len(cs) != 1 {
		return vmEnd{}, fmt.Errorf("%s container inspect %s: not one container's JSON", o.bin, vm)
	}
	c := cs[0]
	e := vmEnd{Running: c.State.Running, Exit: c.State.ExitCode, OOM: c.State.OOMKilled, LimitMB: int(c.HostConfig.Memory >> 20)}
	if o.host != nil && o.host.parent != nil {
		if n, ok := cgroupOOMKills(o.host.parent.path); ok && n > 0 {
			e.OOM = true
		}
		if e.LimitMB == 0 {
			e.LimitMB = cgroupMemoryMaxMB(o.host.parent.path)
		}
	}
	return e, nil
}

// cgroupOOMKills is the oom_kill count in cgroup dir's memory.events (cgroup
// v2): ok false when it cannot be read.
func cgroupOOMKills(dir string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return 0, false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), " "); ok && k == "oom_kill" {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			return n, err == nil
		}
	}
	return 0, false
}

// cgroupMemoryMaxMB is cgroup dir's memory.max in MB: 0 for max or unreadable.
func cgroupMemoryMaxMB(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return int(n >> 20)
}
