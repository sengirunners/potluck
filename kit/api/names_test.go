package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// potluck/1 is read as zm/v0's major; ProtocolMajor itself (zm's work
// adapters compare with it) still tells them apart.
func TestSameProtocol(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"zm/v0", "zm/v0", true},
		{"zm/v0.3", "zm/v0", true},
		{"potluck/1", "zm/v0", true},
		{"potluck/1.2", "zm/v0", true},
		{"zm/v0", "potluck/1", true},
		{"potluck/1", "potluck/1", true},
		{"zm/v9", "zm/v0", false},
		{"potluck/2", "zm/v0", false},
		{"potluck/2", "potluck/1", false},
		{"", "zm/v0", false},
		{"other/1", "zm/v0", false},
	} {
		if got := SameProtocol(c.a, c.b); got != c.same {
			t.Errorf("SameProtocol(%q, %q) = %v", c.a, c.b, got)
		}
	}
	if ProtocolMajor("potluck/1") == ProtocolMajor(Protocol) {
		t.Fatal("ProtocolMajor now aliases potluck/1: zm's adapter check would change")
	}
	// Step 2: the daemon protocol sent is potluck/1, under Potluck-Protocol;
	// Protocol (and AdapterProtocol) keep zm/v0 for zm's work adapters (H7).
	if WireProtocol != "potluck/1" || Protocol != "zm/v0" || AdapterProtocol != "zm/v0" || Spec != "zm/v0" || HeaderNew(HeaderProtocol) != "Potluck-Protocol" {
		t.Fatalf("protocol names: %s %s %s %s", WireProtocol, Protocol, AdapterProtocol, HeaderNew(HeaderProtocol))
	}
	// A step 1 reader compares what it reads with zm/v0 (SameProtocol):
	// what this build sends passes it.
	if !SameProtocol(WireProtocol, "zm/v0") {
		t.Fatal("a step 1 peer would refuse potluck/1")
	}
}

func TestHeaderNames(t *testing.T) {
	if HeaderNew(HeaderAs) != "Potluck-As" || HeaderNew("zm-write-id") != "Potluck-Write-Id" || HeaderNew("Authorization") != "Authorization" {
		t.Fatalf("HeaderNew: %s %s", HeaderNew(HeaderAs), HeaderNew("zm-write-id"))
	}
	h := http.Header{}
	h.Set(HeaderAs, "old")
	if GetHeader(h, HeaderAs) != "old" {
		t.Fatal("old name not read")
	}
	h.Set("Potluck-As", "new")
	if GetHeader(h, HeaderAs) != "new" {
		t.Fatal("new name not read first")
	}
	h.Set("Potluck-As", "")
	if GetHeader(h, HeaderAs) != "old" {
		t.Fatal("an empty new value hides the old one")
	}
	h.Add("Potluck-Team", "a")
	h.Add("Potluck-Team", "b")
	h.Set(HeaderTeam, "c")
	if v := HeaderValues(h, HeaderTeam); len(v) != 2 || v[0] != "a" {
		t.Fatalf("HeaderValues %v", v)
	}
	// SetHeader sends the new name (step 2) and drops a stale old one, so
	// the value it set is what any reader sees.
	h = http.Header{}
	h.Set(HeaderProject, "caller's")
	h.Set("Potluck-Project", "caller's too")
	SetHeader(h, HeaderProject, "mine")
	if h.Get(HeaderProject) != "" || h.Get("Potluck-Project") != "mine" || GetHeader(h, HeaderProject) != "mine" || len(h) != 1 {
		t.Fatalf("SetHeader %v", h)
	}
	SetHeader(h, "Content-Type", "x") // any other name as it is
	if h.Get("Content-Type") != "x" {
		t.Fatalf("SetHeader of another name %v", h)
	}
	h.Del("Content-Type")
	DelHeader(h, HeaderProject)
	if len(h) != 0 {
		t.Fatalf("DelHeader %v", h)
	}
	// SendOldNames: Potluck-* become Zm-*; a Zm-* already set wins.
	h = http.Header{}
	h.Set("Potluck-As", "a")
	h.Set("Potluck-Project", "p")
	h.Set(HeaderProject, "zm-wins")
	h.Set("Content-Type", "x")
	SendOldNames(h)
	if h.Get(HeaderAs) != "a" || h.Get(HeaderProject) != "zm-wins" || h.Get("Content-Type") != "x" {
		t.Fatalf("SendOldNames %v", h)
	}
	for k := range h {
		if strings.HasPrefix(k, "Potluck-") {
			t.Fatalf("SendOldNames left %s", k)
		}
	}
	for _, k := range []string{"Zm-As", "potluck-x", "POTLUCK-Y", "zm-"} {
		if !IsOwnHeader(k) {
			t.Errorf("IsOwnHeader(%q) = false", k)
		}
	}
	for _, k := range []string{"X-Zm-Edge-Secret", "X-Potluck-Edge-Secret", "Content-Type", "Zmx"} {
		if IsOwnHeader(k) {
			t.Errorf("IsOwnHeader(%q) = true", k)
		}
	}
}

func TestNodeNames(t *testing.T) {
	for _, c := range []struct {
		in, prefix, machine string
		ok                  bool
	}{
		{"zm-mini", "zm-", "mini", true},
		{"ZM-Mini", "zm-", "mini", true},
		{"potluck-mini", "potluck-", "mini", true},
		{" Potluck-host-c ", "potluck-", "host-c", true},
		{"mini", "", "", false},
		{"zmrt-1", "", "", false},
		{"potluckmini", "", "", false},
	} {
		p, m, ok := NodeMachine(c.in)
		if p != c.prefix || m != c.machine || ok != c.ok || IsDaemonNode(c.in) != c.ok {
			t.Errorf("NodeMachine(%q) = %q %q %v", c.in, p, m, ok)
		}
	}
}

// A machine's heartbeat may name its build under "potluck" (step 2's) or
// "zm" (step 1's); a strict decoder (an authority of step 1 or 2) takes both.
func TestMachineStatsBuild(t *testing.T) {
	for _, body := range []string{`{"potluck":{"version":"v2"}}`, `{"zm":{"version":"v2"}}`} {
		var st MachineStats
		dec := json.NewDecoder(strings.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&st); err != nil || st.Build() == nil || st.Build().Version != "v2" {
			t.Fatalf("%s: %+v %v", body, st, err)
		}
	}
	b, _ := json.Marshal(MachineStats{Potluck: &MachineZm{Version: "v1"}})
	if strings.Contains(string(b), `"zm"`) || !strings.Contains(string(b), `"potluck":{`) {
		t.Fatalf("what is sent: %s", b)
	}
}

// A machine without sandbox tasks sends no sandbox keys: an older authority
// never sees them.
func TestMachineStatsZeroSendsNoSandbox(t *testing.T) {
	b, _ := json.Marshal(MachineStats{})
	if strings.Contains(string(b), "sandbox") {
		t.Fatalf("zero MachineStats carries sandbox: %s", b)
	}
}
