package vmhost

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// scriptLayer is a vmLayer whose Exec runs the next of a list of shell
// commands, for tests that drive checkRunner without a VM.
type scriptLayer struct {
	vmLayer
	mu    sync.Mutex
	steps []string
}

func (l *scriptLayer) Exec(_ string, stdin io.Reader, stdout, stderr io.Writer, _ ...string) *exec.Cmd {
	l.mu.Lock()
	step := l.steps[0]
	l.steps = l.steps[1:]
	l.mu.Unlock()
	cmd := exec.Command("sh", "-c", step)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd
}

const hang = "exec sleep 60"

func checkWith(t *testing.T, timeout time.Duration, prefetch func(dir string) <-chan hostFetch, steps ...string) (*protocol.RunnerStatus, error) {
	t.Helper()
	dir := t.TempDir()
	s := &sup{st: store{dir}, vm: &scriptLayer{steps: steps}, name: "vm", log: io.Discard}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var pf <-chan hostFetch
	if prefetch != nil {
		pf = prefetch(dir)
	}
	return s.checkRunner(ctx, imgA, "runner", pf)
}

// A refresh that runs out of time says so, wherever the deadline fell: not
// "version unreadable" for the first probe it killed, nor "runner unusable"
// for the check after the install it killed.
func TestRunnerRefreshTimeoutsSayTimedOut(t *testing.T) {
	// The deadline kills the first version probe.
	st, err := checkWith(t, 300*time.Millisecond, nil, hang)
	if err == nil || st == nil || st.Refresh != protocol.RunnerFailed || st.Why != protocol.WhyTimedOut {
		t.Errorf("first probe killed by the deadline: %+v, %v; want failed, %q", st, err, protocol.WhyTimedOut)
	}

	// The install is done; the deadline kills the check that follows it.
	copyFile := func(dir string) <-chan hostFetch {
		p := filepath.Join(dir, "release.tar.gz")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		ch := make(chan hostFetch, 1)
		ch <- hostFetch{plat: "linux-x64", path: p}
		close(ch)
		return ch
	}
	st, err = checkWith(t, 2*time.Second, copyFile, "echo Linux x86_64 2.332.0", "exit 0", hang)
	if err == nil || st == nil || st.Refresh != protocol.RunnerFailed || st.Why != protocol.WhyTimedOut || st.ImageVersion != "2.332.0" {
		t.Errorf("check after the install killed by the deadline: %+v, %v; want failed, %q, image version 2.332.0", st, err, protocol.WhyTimedOut)
	}

	// Without a deadline the same two failures keep their own reasons.
	st, err = checkWith(t, time.Minute, nil, "exit 3")
	if err == nil || st == nil || st.Why != protocol.WhyVersionUnreadable {
		t.Errorf("a probe that fails: %+v, %v; want %q", st, err, protocol.WhyVersionUnreadable)
	}
	st, err = checkWith(t, time.Minute, copyFile, "echo Linux x86_64 2.332.0", "exit 0", "exit 5")
	if err == nil || st == nil || st.Why != protocol.WhyRunnerUnusable {
		t.Errorf("a check that fails: %+v, %v; want %q", st, err, protocol.WhyRunnerUnusable)
	}
}

// Boxes of different images finishing together each keep their result: the
// file is rewritten under its lock, never from a copy read before the other's
// write.
func TestRunnerResultsOfImagesFinishingTogether(t *testing.T) {
	const images, rounds = 24, 5
	for round := 0; round < rounds; round++ {
		st := store{t.TempDir()}
		var ready, done sync.WaitGroup
		start := make(chan struct{})
		ready.Add(images)
		done.Add(images)
		for i := 0; i < images; i++ {
			go func(i int) {
				defer done.Done()
				ready.Done()
				<-start
				st.recordRunner(protocol.RunnerStatus{Image: fmt.Sprintf("img%02d", i), Version: runnerMinVersion, Refresh: protocol.RunnerNotNeeded, At: time.Now().UTC()})
			}(i)
		}
		ready.Wait()
		close(start)
		done.Wait()
		if got := st.readRunners(); len(got) != images {
			t.Fatalf("round %d: %d of %d images kept their result: %v", round, len(got), images, got)
		}
	}
}

// With more images than describe lists, a failing one past the cut still shows.
func TestDescribeRunnersCutKeepsFailures(t *testing.T) {
	st := store{t.TempDir()}
	var images []string
	at := time.Now().UTC()
	for i := 0; i < protocol.MaxRunners+3; i++ {
		im := fmt.Sprintf("img%02d@sha256:%s", i, strings.Repeat("ab", 32))
		images = append(images, im)
		r := protocol.RunnerStatus{Image: im, Version: runnerMinVersion, Refresh: protocol.RunnerNotNeeded, At: at}
		if i == protocol.MaxRunners+1 { // 34th in the binding's order
			r = protocol.RunnerStatus{Image: im, Refresh: protocol.RunnerFailed, Why: protocol.WhyDownloadFailed, At: at}
		}
		st.recordRunner(r)
	}
	got := st.runnerStatuses(images)
	if len(got) != protocol.MaxRunners {
		t.Fatalf("%d results, want %d", len(got), protocol.MaxRunners)
	}
	failed := images[protocol.MaxRunners+1]
	found := false
	for _, r := range got {
		found = found || (r.Image == failed && r.Refresh == protocol.RunnerFailed)
	}
	if !found {
		t.Errorf("the failing image %s was cut: %+v", failed, got)
	}
}

// An image the driver accepts is one describe can report: a reference past the
// reported limit, or with a control character, is refused with the binding.
func TestBoxImageMustBeReportable(t *testing.T) {
	m, _, _ := jitRefreshBox(t, "2.332.0")
	digest := "@sha256:" + strings.Repeat("ab", 32)
	longest := strings.Repeat("a", protocol.MaxRunnerImage-len(digest)) + digest
	m["images"] = longest
	if _, err := parseConfig(m); err != nil {
		t.Errorf("an image of %d bytes is refused: %v", len(longest), err)
	}
	for name, im := range map[string]string{
		"too long":        "a" + longest,
		"control char":    "fake\x1b[31m/linux" + digest,
		"newline in name": "fake\n/linux" + digest,
	} {
		m["images"] = im
		if _, err := parseConfig(m); err == nil {
			t.Errorf("%s: image %q accepted", name, im)
		}
	}
}

// A runner version from the guest that a result may not carry is left out
// (the result then still shows) instead of making the whole result invalid.
func TestRunnerVersionOfDropsWhatCannotBeReported(t *testing.T) {
	for in, want := range map[string]string{"v2.338.2": "2.338.2", "-": "", "": "", strings.Repeat("9", 70): "", "2.1\x1b[0m": ""} {
		if got := runnerVersionOf(in); got != want {
			t.Errorf("runnerVersionOf(%q) = %q, want %q", in, got, want)
		}
	}
}
