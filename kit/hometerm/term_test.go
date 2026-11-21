package hometerm_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/hometerm"
)

var (
	succ = hometerm.Peer{Via: "tailnet", URL: "https://zm-standby.example.ts.net", Fingerprint: "fp-standby"}
	pred = hometerm.Peer{Via: "tunnel", URL: "https://home.example.com", Fingerprint: "fp-home"}
)

func TestLoadNoFileServesAtEpochZero(t *testing.T) {
	home := t.TempDir()
	got, err := hometerm.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got != hometerm.Initial() || !got.Serves() || got.Epoch != 0 {
		t.Fatalf("no file gave %+v, want serving at epoch 0", got)
	}
	if err := hometerm.CheckServes(home); err != nil {
		t.Fatalf("a home with no term file does not serve: %v", err)
	}
}

func TestLoadRefusesWhatIsNotATerm(t *testing.T) {
	for name, body := range map[string]string{
		"empty":           "",
		"not json":        "serving\n",
		"unknown state":   `{"epoch":1,"state":"leader"}`,
		"no state":        `{"epoch":1}`,
		"unknown field":   `{"epoch":1,"state":"serving","leader":true}`,
		"two terms":       `{"epoch":1,"state":"serving"}{"epoch":2,"state":"serving"}`,
		"fenced no succ":  `{"epoch":2,"state":"fenced"}`,
		"standby no pred": `{"epoch":2,"state":"standby"}`,
		"bad via":         `{"epoch":2,"state":"fenced","successor":{"via":"carrier pigeon","url":"u","fingerprint":"f"}}`,
		"no fingerprint":  `{"epoch":2,"state":"fenced","successor":{"via":"tailnet","url":"u"}}`,
		"negative epoch":  `{"epoch":-1,"state":"serving"}`,
		"epoch too high":  `{"epoch":9223372036854775808,"state":"serving"}`,
		"stray brace":     `{"epoch":1,"state":"serving"}}`,
		"trailing text":   `{"epoch":1,"state":"serving"} x`,
		"fenced at 0":     `{"epoch":0,"state":"fenced","successor":{"via":"tailnet","url":"u","fingerprint":"f"}}`,
		"standby w/ succ": `{"epoch":1,"state":"standby","predecessor":{"via":"tailnet","url":"u","fingerprint":"f"},"successor":{"via":"tailnet","url":"v","fingerprint":"g"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, hometerm.File), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := hometerm.Load(home); err == nil {
				t.Fatalf("loaded %+v from %q, want an error", got, body)
			}
			if err := hometerm.CheckServes(home); err == nil {
				t.Fatalf("a home whose term is %q serves", body)
			}
		})
	}
}

func TestLoadUnreadableIsAnError(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, hometerm.File), 0o700); err != nil {
		t.Fatal(err) // a directory in the file's place cannot be read as a file
	}
	if _, err := hometerm.Load(home); err == nil {
		t.Fatal("an unreadable term loaded")
	}
	if err := hometerm.CheckServes(home); err == nil {
		t.Fatal("a home with an unreadable term serves")
	}
}

func TestApplyRoundTripsPrivatelyAndLeavesNoTemp(t *testing.T) {
	home := t.TempDir()
	got, err := hometerm.Apply(home, func(t hometerm.Term) (hometerm.Term, error) { return t.Designate(succ) })
	if err != nil {
		t.Fatal(err)
	}
	if got, err = hometerm.Apply(home, func(t hometerm.Term) (hometerm.Term, error) { return t.Fence(succ, 7) }); err != nil {
		t.Fatal(err)
	}
	loaded, err := hometerm.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if !same(loaded, got) || loaded.Epoch != 7 || loaded.State != hometerm.Fenced || *loaded.Successor != succ {
		t.Fatalf("loaded %+v, want %+v", loaded, got)
	}
	fi, err := os.Stat(filepath.Join(home, hometerm.File))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v, want 0600", fi, err)
	}
	ents, _ := os.ReadDir(home)
	if len(ents) != 1 {
		t.Fatalf("home holds %v, want the term file alone", ents)
	}
}

// Apply holds the rules whatever the move returns: a move that ignores the
// transitions cannot undo a fence, lower the epoch, or write a term that is
// not whole, and a refused move leaves the term as it was.
func TestApplyRefusesAMoveThatBreaksTheRules(t *testing.T) {
	home := t.TempDir()
	if _, err := hometerm.Apply(home, func(t hometerm.Term) (hometerm.Term, error) { return t.Fence(succ, 5) }); err != nil {
		t.Fatal(err)
	}
	for name, next := range map[string]hometerm.Term{
		"serve again at the fenced epoch": {Epoch: 5, State: hometerm.Serving},
		"freeze at the fenced epoch":      {Epoch: 5, State: hometerm.Frozen},
		"lower the epoch":                 {Epoch: 4, State: hometerm.Fenced, Successor: &succ},
		"back to the start":               hometerm.Initial(),
		"not whole":                       {Epoch: 6, State: hometerm.Fenced},
		"become a standby":                {Epoch: 6, State: hometerm.Standby, Predecessor: &pred},
		"fence above the top epoch":       {Epoch: hometerm.MaxEpoch + 1, State: hometerm.Fenced, Successor: &succ},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := hometerm.Apply(home, func(hometerm.Term) (hometerm.Term, error) { return next, nil })
			if !errors.Is(err, hometerm.ErrTransition) {
				t.Fatalf("Apply = %v, want a refused transition", err)
			}
			if got, _ := hometerm.Load(home); got.Epoch != 5 || got.State != hometerm.Fenced {
				t.Fatalf("a refused move changed the term: %+v", got)
			}
		})
	}
	if _, err := hometerm.Apply(home, func(t hometerm.Term) (hometerm.Term, error) { return t.Promote(6, 5) }); err != nil {
		t.Fatalf("a fenced home taking its data back at a higher epoch was refused: %v", err)
	}
}

func TestApplyOnAnUnreadableTermIsAnError(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, hometerm.File), []byte("serving\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	if _, err := hometerm.Apply(home, func(t hometerm.Term) (hometerm.Term, error) { called = true; return t, nil }); err == nil || called {
		t.Fatalf("Apply on an unreadable term = %v (move called %v), want an error before the move", err, called)
	}
}

func TestCreateOnlyOnAHomeWithNoTerm(t *testing.T) {
	home := t.TempDir()
	sb, err := hometerm.NewStandby(pred, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := hometerm.Create(home, sb); err != nil {
		t.Fatal(err)
	}
	if got, err := hometerm.Load(home); err != nil || !same(got, sb) {
		t.Fatalf("loaded %+v %v, want %+v", got, err, sb)
	}
	if err := hometerm.Create(home, hometerm.Initial()); !errors.Is(err, hometerm.ErrTransition) {
		t.Fatalf("Create over an existing term = %v, want refused", err)
	}
	if err := hometerm.Create(t.TempDir(), hometerm.Term{Epoch: 1, State: hometerm.Standby}); err == nil {
		t.Fatal("created a standby term with no predecessor")
	}
}

func TestTransitions(t *testing.T) {
	serving := hometerm.Term{Epoch: 3, State: hometerm.Serving}
	frozen := hometerm.Term{Epoch: 3, State: hometerm.Frozen}
	fenced := hometerm.Term{Epoch: 4, State: hometerm.Fenced, Successor: &succ}
	standby := hometerm.Term{Epoch: 3, State: hometerm.Standby, Predecessor: &pred}
	all := map[string]hometerm.Term{"serving": serving, "frozen": frozen, "fenced": fenced, "standby": standby}

	type move struct {
		name string
		do   func(hometerm.Term) (hometerm.Term, error)
		ok   map[string]hometerm.Term // from-state -> wanted result; every other from-state is refused
	}
	moves := []move{
		{"freeze", hometerm.Term.Freeze, map[string]hometerm.Term{
			"serving": {Epoch: 3, State: hometerm.Frozen},
		}},
		{"unfreeze", hometerm.Term.Unfreeze, map[string]hometerm.Term{
			"frozen": {Epoch: 3, State: hometerm.Serving},
		}},
		{"designate", func(t hometerm.Term) (hometerm.Term, error) { return t.Designate(succ) }, map[string]hometerm.Term{
			"serving": {Epoch: 3, State: hometerm.Serving, Successor: &succ},
			"frozen":  {Epoch: 3, State: hometerm.Frozen, Successor: &succ},
		}},
		{"undesignate", hometerm.Term.Undesignate, map[string]hometerm.Term{
			"serving": {Epoch: 3, State: hometerm.Serving},
			"frozen":  {Epoch: 3, State: hometerm.Frozen},
		}},
		{"fence at 5", func(t hometerm.Term) (hometerm.Term, error) { return t.Fence(succ, 5) }, map[string]hometerm.Term{
			"serving": {Epoch: 5, State: hometerm.Fenced, Successor: &succ},
			"frozen":  {Epoch: 5, State: hometerm.Fenced, Successor: &succ},
			"fenced":  {Epoch: 5, State: hometerm.Fenced, Successor: &succ},
		}},
		{"fence at 4", func(t hometerm.Term) (hometerm.Term, error) { return t.Fence(succ, 4) }, map[string]hometerm.Term{
			"serving": {Epoch: 4, State: hometerm.Fenced, Successor: &succ},
			"frozen":  {Epoch: 4, State: hometerm.Fenced, Successor: &succ},
			// fenced at 4 already: the same epoch never fences again
		}},
		{"fence at 3", func(t hometerm.Term) (hometerm.Term, error) { return t.Fence(succ, 3) }, nil},
		{"promote to 6 over data at 3", func(t hometerm.Term) (hometerm.Term, error) { return t.Promote(6, 3) }, map[string]hometerm.Term{
			"standby": {Epoch: 6, State: hometerm.Serving, Predecessor: &pred},
			"fenced":  {Epoch: 6, State: hometerm.Serving, Predecessor: &succ},
		}},
		{"promote to 4 over data at 4", func(t hometerm.Term) (hometerm.Term, error) { return t.Promote(4, 4) }, nil},
		{"promote to 4 over data at 3", func(t hometerm.Term) (hometerm.Term, error) { return t.Promote(4, 3) }, map[string]hometerm.Term{
			"standby": {Epoch: 4, State: hometerm.Serving, Predecessor: &pred},
			// fenced at 4: not above its own epoch
		}},
	}
	for _, m := range moves {
		for from, term := range all {
			t.Run(m.name+" from "+from, func(t *testing.T) {
				got, err := m.do(term)
				want, ok := m.ok[from]
				if !ok {
					if err == nil {
						t.Fatalf("allowed, gave %+v", got)
					}
					if !errors.Is(err, hometerm.ErrTransition) {
						t.Fatalf("refused with %v, which does not wrap ErrTransition", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if !same(got, want) {
					t.Fatalf("gave %+v, want %+v", got, want)
				}
				if err := got.Check(); err != nil {
					t.Fatalf("result is not whole: %v", err)
				}
			})
		}
	}
}

func TestTransitionsRefuseABadPeer(t *testing.T) {
	bad := hometerm.Peer{Via: "tailnet", URL: "https://x"}
	if _, err := hometerm.Initial().Designate(bad); !errors.Is(err, hometerm.ErrTransition) {
		t.Fatalf("designated a peer with no fingerprint: %v", err)
	}
	if _, err := hometerm.Initial().Fence(bad, 1); !errors.Is(err, hometerm.ErrTransition) {
		t.Fatalf("fenced to a peer with no fingerprint: %v", err)
	}
	if _, err := hometerm.NewStandby(bad, 1); err == nil {
		t.Fatal("a standby follows a peer with no fingerprint")
	}
}

// A standby takes an epoch below MaxEpoch, so a promote above it is left,
// and a promote goes up to MaxEpoch and no further. A successor serving at
// MaxEpoch still fences the home it took over from: that home is retired
// (no promote is above it), never left serving beside its successor.
func TestEpochsAtTheTop(t *testing.T) {
	top := uint64(hometerm.MaxEpoch)
	serving := hometerm.Term{Epoch: 3, State: hometerm.Serving}
	if got, err := hometerm.NewStandby(pred, top); err == nil {
		t.Fatalf("NewStandby at MaxEpoch = %+v, want refused", got)
	}
	if err := (hometerm.Term{Epoch: top, State: hometerm.Standby, Predecessor: &pred}).Check(); err == nil {
		t.Fatal("a standby term at MaxEpoch is whole")
	}
	retired, err := serving.Fence(succ, top)
	if err != nil || retired.State != hometerm.Fenced || retired.Epoch != top {
		t.Fatalf("Fence at MaxEpoch = %+v %v, want fenced there", retired, err)
	}
	if err := retired.Check(); err != nil {
		t.Fatalf("a fenced term at MaxEpoch: %v", err)
	}
	if got, err := retired.Promote(top, 0); !errors.Is(err, hometerm.ErrTransition) {
		t.Fatalf("a home fenced at MaxEpoch promoted = %+v %v, want refused", got, err)
	}
	if got, err := serving.Fence(succ, top+1); !errors.Is(err, hometerm.ErrTransition) {
		t.Fatalf("Fence above MaxEpoch = %+v %v, want refused", got, err)
	}
	fenced, err := serving.Fence(succ, top-1)
	if err != nil {
		t.Fatalf("Fence just below MaxEpoch: %v", err)
	}
	if got, err := fenced.Promote(top, top-1); err != nil || got.Epoch != top || !got.Serves() {
		t.Fatalf("a home fenced just below MaxEpoch promoted to it = %+v %v, want serving at MaxEpoch", got, err)
	}
	if got, err := fenced.Fence(succ, top); err != nil || got.Epoch != top {
		t.Fatalf("a fenced home fenced again at MaxEpoch = %+v %v", got, err)
	}
	sb, err := hometerm.NewStandby(pred, top-1)
	if err != nil {
		t.Fatalf("NewStandby just below MaxEpoch: %v", err)
	}
	if _, err := sb.Promote(top, top-1); err != nil {
		t.Fatalf("a standby just below MaxEpoch promoted to it: %v", err)
	}
	if got, err := sb.Promote(top+1, top-1); !errors.Is(err, hometerm.ErrTransition) {
		t.Fatalf("Promote above MaxEpoch = %+v %v, want refused", got, err)
	}
	// Apply writes the retired term, and Load reads it back.
	home := t.TempDir()
	if _, err := hometerm.Apply(home, func(hometerm.Term) (hometerm.Term, error) { return serving.Fence(succ, top) }); err != nil {
		t.Fatalf("Apply of a fence at MaxEpoch: %v", err)
	}
	if got, err := hometerm.Load(home); err != nil || got.Epoch != top || got.State != hometerm.Fenced {
		t.Fatalf("Load of a term fenced at MaxEpoch = %+v %v", got, err)
	}
}

func TestCheckServesSaysWhatTheHomeIs(t *testing.T) {
	sb, err := hometerm.NewStandby(pred, 2)
	if err != nil {
		t.Fatal(err)
	}
	fz, _ := hometerm.Term{Epoch: 2, State: hometerm.Serving}.Freeze()
	fc, _ := hometerm.Initial().Fence(succ, 1)
	for term, want := range map[*hometerm.Term]string{&sb: "standby", &fz: "frozen", &fc: succ.URL} {
		home := t.TempDir()
		if err := hometerm.Create(home, *term); err != nil {
			t.Fatal(err)
		}
		err := hometerm.CheckServes(home)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s term: CheckServes = %v, want an error naming %q", term.State, err, want)
		}
	}
}

func same(a, b hometerm.Term) bool {
	peq := func(x, y *hometerm.Peer) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Epoch == b.Epoch && a.State == b.State && peq(a.Successor, b.Successor) && peq(a.Predecessor, b.Predecessor)
}

// A serving home that named a successor withdraws it at the same epoch.
func TestUndesignateClearsTheSuccessor(t *testing.T) {
	named, err := hometerm.Term{Epoch: 3, State: hometerm.Serving}.Designate(succ)
	if err != nil {
		t.Fatal(err)
	}
	got, err := named.Undesignate()
	if err != nil || got.Successor != nil || got.Epoch != 3 || got.State != hometerm.Serving {
		t.Fatalf("undesignate: %+v %v", got, err)
	}
}
