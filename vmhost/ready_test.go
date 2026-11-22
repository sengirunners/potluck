package vmhost

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// sayLayer is a layer whose guest answers each boot try as a shell script
// says: the first try runs the first script, and so on, the last one again
// for every try after it.
type sayLayer struct {
	vmLayer
	tries []string
	n     *int
}

func (l sayLayer) Exec(vm string, stdin io.Reader, stdout, stderr io.Writer, argv ...string) *exec.Cmd {
	cmd := exec.Command("sh", "-c", l.tries[min(*l.n, len(l.tries)-1)])
	*l.n++
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd
}

// A guest that answers and cannot give what its binding says tells why on
// its try's stderr. The boot's error carries it, a try that hangs is ended
// at the deadline, and a try that never stops talking is neither waited on
// nor kept whole.
func TestWaitReadySaysWhyAndEndsAtTheDeadline(t *testing.T) {
	for _, c := range []struct {
		name  string
		tries []string
		want  string
	}{
		{"a refusal, then a try that hangs", []string{"echo 'root: the image has no sudo' >&2; exit 127", "exec sleep 60"}, "the guest ran no command within the boot timeout: root: the image has no sudo (a later try was still running when the time passed)"},
		{"a try that hangs and says nothing", []string{"exec sleep 60"}, "the guest ran no command within the boot timeout: its last try was still running when the time passed"},
		{"a try that never stops talking", []string{"while :; do echo noise >&2; done"}, "the guest ran no command within the boot timeout: noise"},
		{"a guest that says nothing", []string{"exit 1"}, "the guest agent did not answer within the boot timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, start := 0, time.Now()
			err := waitReady(sayLayer{tries: c.tries, n: &n}, "vm", start.Add(1500*time.Millisecond), make(chan struct{}))
			if err == nil || !strings.HasPrefix(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if took := time.Since(start); took > 6*time.Second {
				t.Fatalf("the wait took %v past a deadline of 1.5 s", took)
			}
			if len(err.Error()) > readySaid+100 {
				t.Fatalf("the error keeps %d bytes of what the guest said, over %d", len(err.Error()), readySaid)
			}
		})
	}
}

// A try that ran and left something behind holding its output is a guest
// that runs commands: the boot does not wait for what it left.
func TestWaitReadyDoesNotWaitForWhatATryLeftBehind(t *testing.T) {
	n, start := 0, time.Now()
	if err := waitReady(sayLayer{tries: []string{"sleep 8 >&2 & exit 0"}, n: &n}, "vm", start.Add(6*time.Second), make(chan struct{})); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("the boot waited %v for what its try left behind", took)
	}
}
