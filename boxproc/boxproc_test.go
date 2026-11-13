package boxproc

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A stream past KeepBytes keeps at least its newest KeepBytes at absolute
// offsets, and output says how much before them it dropped.
func TestLogKeepsTheNewestAndCountsDropped(t *testing.T) {
	old := segBytes
	segBytes = 64 << 10
	t.Cleanup(func() { segBytes = old })
	dir, err := os.MkdirTemp("", "boxproc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ps := Procs(dir)
	d, err := ps.New("k")
	if err != nil {
		t.Fatal(err)
	}
	total := KeepBytes + 300000
	all := make([]byte, total)
	for i := range all {
		all[i] = byte('a' + i%26)
	}
	if Hold(d, func(stdout, _ io.Writer) (func() int, error) {
		for i := 0; i < total; i += 777 { // writes that straddle segments
			_, _ = stdout.Write(all[i:min(i+777, total)])
		}
		return func() int { return 3 }, nil
	}) != 0 {
		t.Fatal("hold failed")
	}
	ready := func() error { return nil }
	o, err := Output(d, OutputArgs{PID: d.ID(), MaxBytes: 4000}, ready)
	if err != nil {
		t.Fatal(err)
	}
	dropped, _ := o["dropped_stdout"].(int64)
	if dropped <= 0 || dropped > int64(total-KeepBytes) || o["stdout"] != string(all[dropped:dropped+4000]) || o["running"] != false || o["exit"] != 3 {
		t.Fatalf("from 0: dropped %v, running %v, exit %v", o["dropped_stdout"], o["running"], o["exit"])
	}
	from := int64(total - KeepBytes)
	o, err = Output(d, OutputArgs{PID: d.ID(), StdoutFrom: from, MaxBytes: 4000}, ready)
	if err != nil || o["dropped_stdout"] != nil || o["stdout"] != string(all[from:from+4000]) || o["stdout_next"] != from+4000 {
		t.Fatalf("from the newest KeepBytes: %v %v", o["dropped_stdout"], err)
	}
	if _, err := Output(d, OutputArgs{PID: d.ID(), StdoutFrom: int64(total + 1)}, ready); err == nil {
		t.Error("an offset past the end was answered")
	}
	if p, ok := ps.Find("k"); !ok || p != d.ID() {
		t.Errorf("find by request_id: %q %v", p, ok)
	}
}

func TestSplitAndWhole(t *testing.T) {
	o, e := split([]byte(strings.Repeat("o", 100)), []byte(strings.Repeat("e", 100)), 60)
	if len(o) != 30 || len(e) != 30 {
		t.Errorf("both full: %d %d", len(o), len(e))
	}
	o, e = split([]byte(strings.Repeat("o", 100)), []byte("ee"), 60)
	if len(o) != 58 || len(e) != 2 {
		t.Errorf("stderr short: %d %d", len(o), len(e))
	}
	b := []byte("ab\xe2\x82") // a cut-off €
	if whole(b) != 2 || whole([]byte("ab\xe2\x82\xac")) != 5 || whole([]byte("ab")) != 2 {
		t.Errorf("whole: %d", whole(b))
	}
}

func TestParseOutputBounds(t *testing.T) {
	for _, bad := range []string{`{}`, `{"pid":"p","max_bytes":1048577}`, `{"pid":"p","wait_s":21}`, `{"pid":"p","stdout_from":-1}`, `{"pid":"p","encoding":"hex"}`} {
		var r *Refusal
		if _, err := ParseOutput(json.RawMessage(bad)); !errors.As(err, &r) || r.Code != "bad_request" {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if a, err := ParseOutput(json.RawMessage(`{"pid":"p"}`)); err != nil || a.MaxBytes != DefaultBytes {
		t.Errorf("default: %+v %v", a, err)
	}
}

// A process the box remembers past MaxProcs makes room by forgetting the
// oldest ended one; with all of them running, spawn is conflict.
func TestProcsCap(t *testing.T) {
	dir := t.TempDir()
	ps := Procs(dir)
	for i := range MaxProcs {
		d, err := ps.New("k" + string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		m, _ := d.Meta()
		m.Holder = os.Getpid() // alive: running
		_ = d.SetMeta(m)
	}
	var r *Refusal
	if _, err := ps.New("full"); !errors.As(err, &r) || r.Code != "conflict" {
		t.Fatalf("past the cap, all running: %v", err)
	}
	first, _ := ps.Find("ka")
	d, _ := ps.Dir(first)
	_ = writeJSON(string(d)+"/exit.json", map[string]int{"exit": 0})
	time.Sleep(time.Millisecond)
	if _, err := ps.New("room"); err != nil {
		t.Fatalf("with one ended: %v", err)
	}
	if _, ok := ps.Find("ka"); ok {
		t.Error("the ended process is still remembered")
	}
}

// A running stream that ends inside a character is nothing new: output
// waits wait_s rather than answering an empty page at once; once the
// character is whole it comes in one piece, and a max_bytes smaller than a
// character still answers it.
func TestTextPagesWaitOnACutCharacter(t *testing.T) {
	d, err := Procs(t.TempDir()).New("k")
	if err != nil {
		t.Fatal(err)
	}
	more, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		Hold(d, func(stdout, _ io.Writer) (func() int, error) {
			_, _ = stdout.Write([]byte("a\xe2\x82"))
			return func() int {
				<-more
				_, _ = stdout.Write([]byte("\xac\xe2\x82\xacb"))
				return 0
			}, nil
		})
	}()
	ready := func() error { return nil }
	page := func(from int64, max, wait int) (map[string]any, time.Duration) {
		t.Helper()
		start := time.Now()
		o, err := Output(d, OutputArgs{PID: d.ID(), StdoutFrom: from, MaxBytes: max, WaitS: wait}, ready)
		if err != nil {
			t.Fatal(err)
		}
		return o, time.Since(start)
	}
	for o, _ := page(0, 64, 0); o["stdout"] != "a"; o, _ = page(0, 64, 0) {
		time.Sleep(10 * time.Millisecond)
	}
	if o, took := page(1, 64, 1); o["stdout"] != "" || o["stdout_next"] != int64(1) || o["running"] != true || took < 900*time.Millisecond {
		t.Fatalf("a cut character: %v after %s, want an empty page after wait_s", o, took)
	}
	close(more)
	o, _ := page(1, 1, 5) // max_bytes 1: the whole character, never a stall
	if o["stdout"] != "€" || o["stdout_next"] != int64(4) {
		t.Fatalf("max_bytes 1 on a 3-byte character: %v", o)
	}
	<-done
	// Ended: pages cut by max_bytes still never split a character.
	var got strings.Builder
	for from := int64(0); ; {
		o, _ := page(from, 2, 0)
		got.WriteString(o["stdout"].(string))
		if from = o["stdout_next"].(int64); o["running"] == false && from == 8 {
			break
		}
	}
	if got.String() != "a€€b" {
		t.Errorf("2-byte pages of an ended stream: %q", got.String())
	}
}

// output answers an exit whenever it answers not running: the exit landing
// between its two looks at the process must not give "running false, no exit".
func TestOutputNeverAnswersEndedWithoutAnExit(t *testing.T) {
	ps := Procs(t.TempDir())
	for i := range 1000 {
		d, err := ps.New("k" + strings.Repeat("x", i%7) + string(rune('a'+i%26)))
		if err != nil {
			t.Fatal(err)
		}
		if err := d.SetMeta(Meta{Holder: os.Getpid(), Start: time.Now()}); err != nil { // a holder that stays alive
			t.Fatal(err)
		}
		go func() { _ = writeJSON(string(d)+"/exit.json", map[string]int{"exit": 0}) }()
		for {
			o, err := Output(d, OutputArgs{MaxBytes: 1}, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if o["running"] == true {
				continue
			}
			if _, ok := o["exit"]; !ok {
				t.Fatalf("round %d: output answers running false and no exit", i)
			}
			break
		}
		_ = os.RemoveAll(string(d))
	}
}

// Two writers of one process's record (the spawner and its holder both write
// meta.json) never fail each other: each write lands whole or not at all.
func TestConcurrentSetMetaNeverFails(t *testing.T) {
	d, err := Procs(t.TempDir()).New("k")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8*200)
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if err := d.SetMeta(Meta{RequestID: "k", Holder: w + 1, Start: time.Now()}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("SetMeta: %v", err)
	}
	if m, err := d.Meta(); err != nil || m.RequestID != "k" {
		t.Fatalf("meta %+v, %v", m, err)
	}
}
