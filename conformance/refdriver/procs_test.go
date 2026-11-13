package refdriver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/boxproc"
)

func TestMain(m *testing.M) {
	if code, handled := Main(); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// The spawning driver already owns this state lock when it starts the
// detached holder. Both normal and PTY holders must wait for that parent to
// publish Holder before reading fresh metadata and publishing Group.
func TestHolderMetadataPublicationWaitsForSpawnLock(t *testing.T) {
	for _, pty := range []bool{false, true} {
		t.Run(fmt.Sprintf("pty=%v", pty), func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			r := record{Handle: "box:test/h1", Kind: "box", State: "ready", Dir: t.TempDir(), Deadline: time.Now().Add(time.Minute)}
			if err := save(state, r); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { KillAll(state) })
			d := drv{state: state}
			ps := procsOf(state, r.Handle)
			if err := os.MkdirAll(string(ps), 0o700); err != nil {
				t.Fatal(err)
			}
			var pd boxproc.Dir
			var holder int
			err := locked(state, func() error {
				var err error
				pd, err = ps.New("publication")
				if err != nil {
					return err
				}
				spec := boxproc.Spec{Argv: []string{"/bin/sh", "-c", "exit 0"}}
				if pty {
					spec.PTY = &boxproc.PTY{Rows: 24, Cols: 80, Term: "xterm"}
					m, _ := pd.Meta()
					m.PTY = true
					if err := pd.SetMeta(m); err != nil {
						return err
					}
				}
				raw, _ := json.Marshal(spec)
				holder, err = d.startHolder(r.Handle, pd.ID(), raw)
				if err != nil {
					return err
				}
				// The child's exec mark proves the holder reached the publication
				// boundary. An already published Group/exit is also evidence it passed it.
				until := time.Now().Add(3 * time.Second)
				for {
					marks, _ := filepath.Glob(filepath.Join(r.Dir, ".execs", "*"))
					m, _ := pd.Meta()
					if len(marks) > 0 || m.Group > 0 || pd.Exit() != nil {
						break
					}
					if time.Now().After(until) {
						return fmt.Errorf("holder never reached metadata publication")
					}
					time.Sleep(5 * time.Millisecond)
				}
				for until := time.Now().Add(150 * time.Millisecond); time.Now().Before(until); time.Sleep(5 * time.Millisecond) {
					m, err := pd.Meta()
					if err != nil {
						return err
					}
					if m.Group != 0 || pd.Exit() != nil {
						return fmt.Errorf("holder published Group=%d/exit=%v while spawning driver still owns state lock", m.Group, pd.Exit())
					}
				}
				m, err := pd.Meta()
				if err != nil {
					return err
				}
				m.Holder = holder
				return pd.SetMeta(m)
			})
			if err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(3 * time.Second)
			for pd.Exit() == nil && time.Now().Before(until) {
				time.Sleep(5 * time.Millisecond)
			}
			if code := pd.Exit(); code == nil || *code != 0 {
				t.Fatalf("holder failed after lock release: exit=%v", code)
			}
			m, err := pd.Meta()
			if err != nil {
				t.Fatal(err)
			}
			if m.Holder != holder || m.Group <= 0 || m.PTY != pty {
				t.Fatalf("publication lost parent/holder fields: %+v; holder=%d pty=%v", m, holder, pty)
			}
		})
	}
}

// The spawner publishes Holder and the holder publishes Group, each by
// reading meta.json and writing it back. Raced, neither may lose the other's
// field.
func TestHolderAndSpawnerMetadataRaceKeepsBothFields(t *testing.T) {
	state := t.TempDir()
	d := drv{state: state}
	const rounds = 40
	for i := 0; i < rounds; i++ {
		// A box of its own per round. The fake Holder pid below may be a live
		// process on the machine running the test, which makes the box count
		// the round as running; sharing one box would then reach MaxProcs.
		ps := procsOf(state, fmt.Sprintf("box:test/race-%d", i))
		if err := os.MkdirAll(string(ps), 0o700); err != nil {
			t.Fatal(err)
		}
		pd, err := ps.New("race")
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var spawnErr error
		wg.Add(2)
		go func() { // the spawning driver, as in spawn
			defer wg.Done()
			<-start
			spawnErr = locked(state, func() error {
				m, _ := pd.Meta()
				m.Holder = 1234
				return pd.SetMeta(m)
			})
		}()
		go func() { // the holder, as in hold and holdPTY
			defer wg.Done()
			<-start
			d.publishGroup(pd, 5678)
		}()
		close(start)
		wg.Wait()
		if spawnErr != nil {
			t.Fatal(spawnErr)
		}
		m, err := pd.Meta()
		if err != nil {
			t.Fatal(err)
		}
		if m.Holder != 1234 || m.Group != 5678 {
			t.Fatalf("round %d lost a field: Holder=%d Group=%d, want 1234 and 5678", i, m.Holder, m.Group)
		}
	}
}
