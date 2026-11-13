package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A health answer's Parts are written at the top level after its fields, in
// key order, the same through json.Marshal and the encoder WriteJSON uses;
// decoding keeps the fields and drops the parts.
func TestHealthPartsAtTopLevel(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	base := Health{OK: true, Protocol: WireProtocol, Version: "v", Commit: "c", Machine: "mini", PID: 7, Started: at,
		Hedges: &HedgeStats{Reads: 3}, LastSwap: &SwapRecord{}}
	h := base
	h.Parts = map[string]json.RawMessage{"holders": json.RawMessage(`{"adopted":2}`), "backup": json.RawMessage(`{"kind":"file","error":"gone"}`)}
	type plain Health
	fields, _ := json.Marshal(plain(base))
	want := string(fields[:len(fields)-1]) + `,"backup":{"kind":"file","error":"gone"},"holders":{"adopted":2}}`
	m, err := json.Marshal(h)
	if err != nil || string(m) != want {
		t.Fatalf("marshal %s %v\nwant %s", m, err, want)
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(h); err != nil || strings.TrimSuffix(buf.String(), "\n") != want {
		t.Fatalf("encode %s %v", buf.String(), err)
	}
	var back Health
	if err := json.Unmarshal(m, &back); err != nil || back.Machine != "mini" || back.Hedges == nil || back.Parts != nil {
		t.Fatalf("decoded %+v %v", back, err)
	}
	if HealthKeyTaken("holders") || !HealthKeyTaken("machine") {
		t.Fatal("a part's key is taken, or a field's is free")
	}
	// No parts: the plain fields.
	if got, _ := json.Marshal(base); string(got) != string(fields) {
		t.Fatalf("no parts: %s, want %s", got, fields)
	}
}
