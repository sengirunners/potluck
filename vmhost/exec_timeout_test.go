package vmhost

import "testing"

// exec stopped at its timeout answers timed_out and the timeout_s it ran
// under; a command that exits 124 by itself does not.
func TestExecAnswersTimedOut(t *testing.T) {
	m, _, h, _ := ociBox(t)
	x, err := call(t, m, "exec", h, map[string]any{"argv": []string{"sleep", "10"}, "timeout_s": 1})
	if err != nil || x["exit"] != float64(124) || x["timed_out"] != true || x["timeout_s"] != float64(1) {
		t.Fatalf("exec past its timeout: %v %v", x, err)
	}
	x, err = call(t, m, "exec", h, map[string]any{"argv": []string{"sh", "-c", "exit 124"}, "timeout_s": 5})
	if err != nil || x["exit"] != float64(124) || x["timed_out"] != nil || x["timeout_s"] != nil {
		t.Fatalf("a command's own 124: %v %v", x, err)
	}
}
