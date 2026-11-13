package runtarget

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

// The host's cap measures an answer as its report encodes it: 2 MiB of '<'
// a driver wrote raw is 12 MiB once escaped, and comes back cut to fit and
// truncated, not as an error (zm-eakh.12.5.10.3).
func TestRunTargetCapCountsEscapes(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"exit": 0, "stdout": strings.Repeat("<", 2<<20), "stderr": ""})
	raw = bytes.ReplaceAll(raw, []byte(`\u003c`), []byte("<")) // as a driver that does not escape HTML writes it
	out := capAnswer("exec", raw)
	if out == nil {
		t.Fatal("no answer")
	}
	rep, _ := json.Marshal(api.MachineOpReport{ID: "x", Payload: out})
	var ex computeapi.RunTargetExec
	if len(rep) > 3<<20+512 || json.Unmarshal(out, &ex) != nil || !ex.Truncated || len(ex.Stdout) == 0 || strings.Trim(ex.Stdout, "<") != "" {
		t.Fatalf("encoded report %d bytes, truncated=%v, stdout %d bytes", len(rep), ex.Truncated, len(ex.Stdout))
	}
	if small := capAnswer("status", []byte(`{"state":"ready"}`)); string(small) != `{"state":"ready"}` {
		t.Errorf("a small answer: %s", small)
	}
	if capAnswer("status", raw) != nil {
		t.Error("an oversized non-exec answer was kept")
	}
}
