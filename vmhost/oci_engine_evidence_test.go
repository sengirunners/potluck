package vmhost

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func fakeEngineEvidence(t *testing.T) (dockerEngineOwner, string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.Mkdir(bin, 0700)
	for name, body := range map[string]string{
		"sudo":       "#!/bin/sh\n[ \"$1\" = -n ] || exit 2\nshift\nexec \"$@\"\n",
		"journalctl": "#!/bin/sh\nexec \"$ENGINE_TEST_EXE\" --fake-native-journal \"$@\"\n",
		"systemctl":  "#!/bin/sh\ncat \"$ENGINE_SYSTEMD\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	journal := filepath.Join(dir, "journal")
	systemd := filepath.Join(dir, "systemd")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENGINE_TEST_EXE", exe)
	t.Setenv("ENGINE_JOURNAL", journal)
	t.Setenv("ENGINE_SYSTEMD", systemd)
	owner := dockerEngineOwner{pid: 1817, invocation: strings.Repeat("a", 32), boot: strings.Repeat("b", 32), started: 100}
	if err := os.WriteFile(systemd, []byte("MainPID=1817\nInvocationID="+owner.invocation+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return owner, journal, systemd
}
func journalEngineRecord(t *testing.T, owner dockerEngineOwner, at uint64, message string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"_PID": strconv.Itoa(owner.pid), "_BOOT_ID": owner.boot, "_SYSTEMD_INVOCATION_ID": owner.invocation, "__MONOTONIC_TIMESTAMP": fmt.Sprint(at), "MESSAGE": message, "_SYSTEMD_UNIT": "docker.service"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// This is the existing Docker29 successful-reload JSON/logfmt shape, including
// hidden shim-v2 registration/options. Only selected runtime data is retained.
func TestNativeProfileUsesCurrentSuccessfulDockerJournal(t *testing.T) {
	for _, mode := range []string{"valid", "old-gap-recovered", "indexed-audit-noise", "pre-start", "journal-live-conflict", "pending", "missing-generation", "missing-runtime", "failed", "marshal-error", "journal-gap", "wrong-boot", "wrong-pid", "wrong-invocation", "restart", "out-of-order", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			profile, p := testHostProfile(t)
			_, dir := newFakeOCI(t, "none")
			if err := os.WriteFile(filepath.Join(dir, "runtimes"), []byte(`{"kata-qemu":{}}`), 0600); err != nil {
				t.Fatal(err)
			}
			owner, journal, systemd := fakeEngineEvidence(t)
			old := verifyDockerPeer
			verifyDockerPeer = func(_ net.Conn) (dockerEngineOwner, error) { return owner, nil }
			t.Cleanup(func() { verifyDockerPeer = old })
			raw, err := os.ReadFile(filepath.Join(filepath.Dir(p.ConfigPath), "daemon.json"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "journal-live-conflict" {
				raw = []byte(`{"runtimes":{"kata-qemu":{"path":"/other/runsc"}}}`)
			}
			message := `time="2026-11-12T18:55:27Z" level=info msg="Reloaded configuration" config=` + strconv.Quote(string(raw))
			recordOwner := owner
			switch mode {
			case "wrong-boot":
				recordOwner.boot = strings.Repeat("c", 32)
			case "wrong-pid":
				recordOwner.pid++
			case "wrong-invocation":
				recordOwner.invocation = strings.Repeat("d", 32)
			}
			older := `level=info msg="Reloaded configuration" config=` + strconv.Quote(`{"runtimes":{"runsc":{"path":"/usr/local/bin/runsc"}}}`)
			records := journalEngineRecord(t, owner, 140, `level=info msg="Got signal to reload configuration"`) + journalEngineRecord(t, owner, 150, older) + journalEngineRecord(t, recordOwner, 199, `level=info msg="Got signal to reload configuration"`) + journalEngineRecord(t, recordOwner, 200, message)
			switch mode {
			case "pending":
				records += journalEngineRecord(t, owner, 201, `level=info msg="Got signal to reload configuration"`)
			case "missing-generation":
				records += journalEngineRecord(t, owner, 201, `level=info msg="Reloaded configuration"`)
			case "missing-runtime":
				records += journalEngineRecord(t, owner, 201, `level=info msg="Reloaded configuration" config="{\"runtimes\":{}}"`)
			case "failed":
				records += journalEngineRecord(t, owner, 201, `level=error msg="Error reloading configuration"`)
			case "marshal-error":
				records += journalEngineRecord(t, owner, 201, `level=error msg="Error when marshaling configuration for printing"`)
			case "journal-gap":
				records += journalGapRecord(t, owner, 201, "Suppressed 8 messages from docker.service")
			case "old-gap-recovered":
				records = journalGapRecord(t, owner, 130, "Suppressed 8 messages from docker.service") + records
			case "pre-start":
				records = journalEngineRecord(t, owner, 99, message) + records
			case "indexed-audit-noise":
				for i := 0; i < 140; i++ {
					records += journalNoiseRecord(t, owner, uint64(300+i))
				}
			case "out-of-order":
				records += journalEngineRecord(t, owner, 199, message)
			case "malformed":
				records = "not journal JSON\n"
			case "restart":
				_ = os.WriteFile(systemd, []byte("MainPID=1818\nInvocationID="+owner.invocation+"\n"), 0600)
			}
			if err := os.WriteFile(journal, []byte(records), 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadOCIHostProfile(profile, "kata-qemu")
			if mode != "valid" && mode != "old-gap-recovered" && mode != "indexed-audit-noise" {
				if err == nil {
					t.Fatal("unverifiable active generation accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b, err := loaded.HostBudget(1, 2048); err != nil || b != (HostBudget{2, 2560}) {
				t.Fatalf("supported native allocation: %+v %v", b, err)
			}
		})
	}
}

func journalGapRecord(t *testing.T, owner dockerEngineOwner, at uint64, message string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"_PID": "91", "_BOOT_ID": owner.boot, "_SYSTEMD_INVOCATION_ID": "journald", "__MONOTONIC_TIMESTAMP": fmt.Sprint(at), "MESSAGE": message, "_TRANSPORT": "driver", "SYSLOG_IDENTIFIER": "systemd-journald"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}
func journalNoiseRecord(t *testing.T, owner dockerEngineOwner, at uint64) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"_PID": "501", "_BOOT_ID": owner.boot, "_SYSTEMD_UNIT": "ssh.service", "_SYSTEMD_INVOCATION_ID": "sudo-audit", "__MONOTONIC_TIMESTAMP": fmt.Sprint(at), "MESSAGE": "sudo journalctl --grep=Reloaded configuration"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// Model journalctl's documented MATCH disjunction, grep/line-induced reverse,
// field projection and bounded tail. Fixture storage is oldest-first; malformed
// order/data fixtures remain malformed, rather than being silently sorted.
func fakeNativeJournal(args []string) int {
	groups := [][]string{{}}
	reverse, grepFlag := false, false
	limit := 10
	oldest := false
	pattern := ""
	fields := map[string]bool{}
	for _, a := range args {
		switch {
		case a == "+":
			groups = append(groups, []string{})
		case a == "--reverse":
			reverse = true
		case strings.HasPrefix(a, "--grep="):
			grepFlag = true
			pattern = strings.TrimPrefix(a, "--grep=")
		case strings.HasPrefix(a, "--lines="):
			n := strings.TrimPrefix(a, "--lines=")
			oldest = strings.HasPrefix(n, "+")
			var err error
			limit, err = strconv.Atoi(strings.TrimPrefix(n, "+"))
			if err != nil || limit < 1 {
				return 2
			}
		case strings.HasPrefix(a, "--output-fields="):
			for _, f := range strings.Split(strings.TrimPrefix(a, "--output-fields="), ",") {
				fields[f] = true
			}
		case !strings.HasPrefix(a, "-") && strings.Contains(a, "="):
			groups[len(groups)-1] = append(groups[len(groups)-1], a)
		}
	}
	if grepFlag && !oldest {
		reverse = true
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return 2
	}
	b, err := os.ReadFile(os.Getenv("ENGINE_JOURNAL"))
	if err != nil {
		return 2
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var e map[string]string
		if json.Unmarshal([]byte(line), &e) != nil {
			lines = append(lines, line)
			continue
		}
		if !re.MatchString(e["MESSAGE"]) {
			continue
		}
		matched := false
		for _, group := range groups {
			all := true
			for _, match := range group {
				k, v, _ := strings.Cut(match, "=")
				if e[k] != v {
					all = false
					break
				}
			}
			matched = matched || all
		}
		if !matched {
			continue
		}
		if len(fields) > 0 {
			for k := range e {
				if !fields[k] && k != "_BOOT_ID" && k != "__MONOTONIC_TIMESTAMP" {
					delete(e, k)
				}
			}
			raw, _ := json.Marshal(e)
			line = string(raw)
		}
		lines = append(lines, line)
	}
	if len(lines) > limit {
		if oldest {
			lines = lines[:limit]
		} else {
			lines = lines[len(lines)-limit:]
		}
	}
	if reverse {
		for i := len(lines) - 1; i >= 0; i-- {
			fmt.Println(lines[i])
		}
	} else {
		for _, line := range lines {
			fmt.Println(line)
		}
	}
	return 0
}
