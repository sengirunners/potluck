package server_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/hometerm"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/wiring"
)

// A daemon serves only a home whose term is serving: a home fenced for its
// successor, frozen for a move or kept as a standby does not start, and
// opens no store.
func TestDaemonServesOnlyAServingTerm(t *testing.T) {
	succ := hometerm.Peer{Via: "tailnet", URL: "https://zm-next.example.ts.net", Fingerprint: "fp-next"}
	fenced, _ := hometerm.Initial().Fence(succ, 1)
	frozen, _ := hometerm.Initial().Freeze()
	standby, _ := hometerm.NewStandby(succ, 1)
	serving, _ := hometerm.Initial().Designate(succ)
	for _, tc := range []struct {
		term   *hometerm.Term
		serves bool
	}{{nil, true}, {&serving, true}, {&fenced, false}, {&frozen, false}, {&standby, false}, {&hometerm.Term{State: "corrupt"}, false}} {
		name := "no term file"
		if tc.term != nil {
			name = string(tc.term.State)
		}
		t.Run(name, func(t *testing.T) {
			home := shortHome(t)
			switch {
			case tc.term == nil:
			case tc.term.State == "corrupt": // a term the daemon cannot read
				if err := os.WriteFile(filepath.Join(home, hometerm.File), []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := hometerm.Create(home, *tc.term); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := config.LoadFrom(home)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Human, cfg.Machine = "owner", "host"
			dm, err := New(context.Background(), cfg, DiscardLog(), wiring.Kit)
			if tc.serves {
				if err != nil {
					t.Fatalf("a %s home did not start: %v", name, err)
				}
				dm.Close()
				return
			}
			if err == nil {
				dm.Close()
				t.Fatalf("a %s home started", name)
			}
			want := string(tc.term.State)
			if want == "corrupt" {
				want = "until its term can be read"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal %q does not say %q", err, want)
			}
			if _, err := os.Stat(filepath.Join(home, store.FileRegistry)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a %s home's store was opened (%v)", name, err)
			}
			if owner := config.HomeOwner(home); owner != "" {
				t.Fatalf("a %s home was claimed by %q", name, owner)
			}
		})
	}
}
