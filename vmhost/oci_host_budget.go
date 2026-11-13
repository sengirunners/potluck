package vmhost

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// hostBudget is the one total charged to admission and enforced on the host.
// OCI's CPU/memory remain the workload's budget, not this larger total.
type hostBudget struct{ CPU, MemoryMiB int }
type HostBudget = hostBudget

// LoadOCIHostProfile shares the resource producer with the lending planner.
func LoadOCIHostProfile(path, runtime string) (*ociHostProfile, error) {
	return loadOCIHostProfile(path, runtime)
}
func (p *ociHostProfile) HostBudget(cpu, memory int) (HostBudget, error) {
	return p.budget(cpu, memory)
}

func (d *drv) requestedHostBudget() (hostBudget, error) {
	if d.budgetLoaded {
		return d.budget, d.budgetErr
	}
	d.budgetLoaded = true
	d.budget = hostBudget{d.cfg.CPU, d.cfg.Memory}
	if d.cfg.VM == "oci" {
		id, err := inspectDockerRuntime(d.cfg.OCICLI, d.cfg.OCIRuntime)
		if err != nil {
			d.budgetErr = err
			return d.budget, err
		}
		d.nativeRuntime = id.kata
		d.engine = &id
		if !id.kata && d.cfg.OCIHostProfile == "" {
			return d.budget, nil
		}
		if d.cfg.Warm > 0 {
			d.budgetErr = fmt.Errorf("bounded OCI resources cannot create unleased warm boxes")
			return d.budget, d.budgetErr
		}
		if d.cfg.OCIHostProfile == "" {
			d.budgetErr = fmt.Errorf("Kata requires a verified oci_host_profile")
			return d.budget, d.budgetErr
		}
		p, err := loadOCIHostProfile(d.cfg.OCIHostProfile, d.cfg.OCIRuntime)
		d.budgetErr = err
		if err == nil {
			d.budget, d.budgetErr = p.budget(d.cfg.CPU, d.cfg.Memory)
		}
	}
	return d.budget, d.budgetErr
}

// ociHostProfile is owner configuration, never renter input. Its allowances
// include native VM overhead and host processes once. Integer CPU ceilings
// deliberately match the existing admission contract, not QEMU topology.
type ociHostProfile struct {
	endpoint          string
	Runtime           string `json:"runtime"`
	ConfigPath        string `json:"runtime_config"`
	ConfigSHA256      string `json:"runtime_config_sha256"`
	BinarySHA256      string `json:"runtime_binary_sha256"`
	CPUOverhead       int    `json:"cpu_overhead"`
	MemoryOverheadMiB int    `json:"memory_overhead_mib"`
	CleanupReserveS   int    `json:"cleanup_reserve_s"`
}

func loadOCIHostProfile(path, runtime string) (*ociHostProfile, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("oci_host_profile must be an absolute owner profile path")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("oci_host_profile: %w", err)
	}
	var p ociHostProfile
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return nil, fmt.Errorf("oci_host_profile: %w", err)
	}
	if p.Runtime != runtime || !filepath.IsAbs(p.ConfigPath) || p.CPUOverhead < 1 || p.MemoryOverheadMiB < 1 || p.CleanupReserveS < 10 || p.CleanupReserveS > 30 {
		return nil, fmt.Errorf("oci_host_profile needs the selected runtime, an absolute runtime_config, positive total overhead allowances, and cleanup_reserve_s 10..30")
	}
	b, err = p.verifyRuntime()
	if err != nil {
		return nil, err
	}
	// This deliberately accepts the packaged scalar settings, not an arbitrary
	// TOML dialect. Unknown/duplicate required values refuse verification.
	values := map[string]string{}
	section := ""
	s := bufio.NewScanner(strings.NewReader(string(b)))
	for s.Scan() {
		line := strings.TrimSpace(strings.SplitN(s.Text(), "#", 2)[0])
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if section == "[runtime]" && (key == "static_sandbox_resource_mgmt" || key == "sandbox_cgroup_only" || key == "enable_vcpus_pinning") || strings.HasPrefix(section, "[hypervisor.") && (key == "overhead_vcpus" || key == "overhead_memory") {
			if _, duplicate := values[key]; duplicate {
				return nil, fmt.Errorf("ambiguous native resource setting %s", key)
			}
			values[key] = value
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	cpu, e1 := strconv.ParseFloat(values["overhead_vcpus"], 64)
	mem, e2 := strconv.Atoi(values["overhead_memory"])
	if e1 != nil || e2 != nil || math.IsNaN(cpu) || math.IsInf(cpu, 0) || cpu < 0 || mem < 0 || float64(p.CPUOverhead) < cpu || p.MemoryOverheadMiB <= mem || values["static_sandbox_resource_mgmt"] != "true" || values["sandbox_cgroup_only"] != "true" || values["enable_vcpus_pinning"] != "false" {
		return nil, fmt.Errorf("native overhead/grouping is unverified or does not fit the configured total host allowance")
	}
	return &p, nil
}

func (p *ociHostProfile) verifyRuntime() ([]byte, error) {
	b, err := os.ReadFile(p.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("runtime configuration: %w", err)
	}
	sum := sha256.Sum256(b)
	if len(p.ConfigSHA256) != 64 || hex.EncodeToString(sum[:]) != p.ConfigSHA256 {
		return nil, fmt.Errorf("runtime configuration changed: oci_host_profile SHA256 does not match")
	}
	id, err := inspectPinnedDockerRuntime("docker", p.Runtime, p.endpoint)
	if err != nil {
		return nil, err
	}
	p.endpoint = id.endpoint
	r := id.runtime
	var configPath string
	if len(r.Options) != 1 || json.Unmarshal(r.Options["ConfigPath"], &configPath) != nil || configPath != p.ConfigPath || !id.kata {
		return nil, fmt.Errorf("active Docker runtime does not expose the exact profile ConfigPath/options and Kata identity")
	}
	bin, err := os.ReadFile(r.Type)
	if err != nil {
		return nil, err
	}
	binarySum := sha256.Sum256(bin)
	if len(p.BinarySHA256) != 64 || hex.EncodeToString(binarySum[:]) != p.BinarySHA256 {
		return nil, fmt.Errorf("selected Docker runtime binary changed or is unverified")
	}
	return b, nil
}

func (p *ociHostProfile) budget(cpu, memory int) (hostBudget, error) {
	if cpu <= 0 || memory <= 0 || cpu > math.MaxInt-p.CPUOverhead || memory > math.MaxInt-p.MemoryOverheadMiB || int64(cpu+p.CPUOverhead) > math.MaxInt64/1_000_000 || int64(memory+p.MemoryOverheadMiB) > math.MaxInt64/(1024*1024) {
		return hostBudget{}, fmt.Errorf("host budget needs finite positive workload resources")
	}
	return hostBudget{cpu + p.CPUOverhead, memory + p.MemoryOverheadMiB}, nil
}
