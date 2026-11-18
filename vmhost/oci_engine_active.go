package vmhost

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Docker's current /info omits shim Type/Options. Docker's successful reload
// record serializes config.Sanitize(daemon.Config()), AFTER activation. Use
// only the current service invocation and require its latest reload to succeed.
// The indexed native query explicitly returns newest first. Validate its
// history, but only the newest relevant event decides the active generation.
// Native journald driver records are a separate indexed OR branch: suppression
// messages need not carry Docker's unit or invocation metadata.
// Missing history, a pending/failed reload, or an identity change refuses.
func activeDockerRuntime(owner dockerEngineOwner, runtime string) (dockerRuntime, error) {
	var zero dockerRuntime
	if owner.pid <= 1 || owner.invocation == "" || owner.boot == "" {
		return zero, fmt.Errorf("live Docker shim options are hidden and active configuration evidence is unavailable")
	}
	// sudo -n -k: as the ownership read (oci_engine_owner.go), a ticket
	// cached at this terminal is ignored, as the daemon's driver has none. A
	// refusal wraps ErrOCIHostNeedsSudo; the journal's output is never shown.
	b, err := sudoNK("journalctl", "--boot", "--reverse", "--lines=128", "--no-pager", "--output=json", "--output-fields=_PID,_BOOT_ID,_SYSTEMD_INVOCATION_ID,MESSAGE,__MONOTONIC_TIMESTAMP",
		"--grep=Reloaded configuration|Got signal to reload configuration|Error reloading configuration|Error when marshaling configuration for printing|Suppressed .*docker.service|Missed .*docker.service",
		"_BOOT_ID="+owner.boot, "_SYSTEMD_UNIT=docker.service", "_SYSTEMD_INVOCATION_ID="+owner.invocation, "_PID="+strconv.Itoa(owner.pid), "+",
		"_BOOT_ID="+owner.boot, "_TRANSPORT=driver", "SYSLOG_IDENTIFIER=systemd-journald")
	if err != nil {
		return zero, fmt.Errorf("read active Docker configuration evidence: %w", err)
	}
	if len(b) > 4<<20 {
		return zero, fmt.Errorf("Docker reload evidence exceeds bounded history")
	}
	var selected dockerRuntime
	valid := false
	var last uint64
	seenTime, decided := false, false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var e struct {
			PID        string `json:"_PID"`
			Boot       string `json:"_BOOT_ID"`
			Invocation string `json:"_SYSTEMD_INVOCATION_ID"`
			Mono       string `json:"__MONOTONIC_TIMESTAMP"`
			Message    string `json:"MESSAGE"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return zero, fmt.Errorf("unreadable Docker reload evidence")
		}
		gap := e.Boot == owner.boot && (strings.Contains(e.Message, "Suppressed ") || strings.Contains(e.Message, "Missed ")) && strings.Contains(e.Message, "docker.service")
		current := e.PID == strconv.Itoa(owner.pid) && e.Boot == owner.boot && e.Invocation == owner.invocation
		if !gap && !current {
			continue
		}
		at, err := strconv.ParseUint(e.Mono, 10, 64)
		if err != nil {
			return zero, fmt.Errorf("Docker reload evidence timestamp is unverifiable")
		}
		if gap && at < owner.started {
			continue
		}
		if at < owner.started || seenTime && at > last {
			return zero, fmt.Errorf("Docker reload evidence ordering is unverifiable")
		}
		last, seenTime = at, true
		if gap {
			if !decided {
				decided = true
				valid = false
			}
			continue
		}
		fields, err := dockerLogFields(e.Message)
		if err != nil {
			return zero, err
		}
		if fields["msg"] != "Reloaded configuration" {
			if !decided {
				decided = true
				valid = false
			}
			continue
		}
		var cfg struct {
			Runtimes map[string]dockerRuntime `json:"runtimes"`
		}
		if err := json.Unmarshal([]byte(fields["config"]), &cfg); err != nil {
			return zero, fmt.Errorf("successful Docker reload omitted active configuration")
		}
		if !decided {
			selected, valid = cfg.Runtimes[runtime]
			decided = true
		}
	}
	if !valid {
		return zero, fmt.Errorf("no complete successful current-invocation Docker reload proves the active shim/options")
	}
	// Do not accept evidence if the service restarted while it was being read.
	now, err := boundedHostCommand(2*time.Second, "systemctl", "show", "docker.service", "-p", "MainPID", "-p", "InvocationID")
	if err != nil {
		return zero, err
	}
	p := systemdProperties(string(now))
	if p["MainPID"] != strconv.Itoa(owner.pid) || p["InvocationID"] != owner.invocation {
		return zero, fmt.Errorf("Docker service identity changed during verification")
	}
	return selected, nil
}

func dockerLogFields(s string) (map[string]string, error) {
	fields := map[string]string{}
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		key, rest, ok := strings.Cut(s, "=")
		if !ok || strings.ContainsAny(key, " \t\n") {
			return nil, fmt.Errorf("unreadable Docker reload log fields")
		}
		var value string
		if strings.HasPrefix(rest, "\"") {
			q, err := strconv.QuotedPrefix(rest)
			if err != nil {
				return nil, err
			}
			value, err = strconv.Unquote(q)
			if err != nil {
				return nil, err
			}
			s = rest[len(q):]
		} else {
			value, s, _ = strings.Cut(rest, " ")
		}
		if _, ok := fields[key]; ok {
			return nil, fmt.Errorf("duplicate Docker reload log field")
		}
		fields[key] = value
	}
	return fields, nil
}

func systemdProperties(s string) map[string]string {
	p := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			p[k] = v
		}
	}
	return p
}
