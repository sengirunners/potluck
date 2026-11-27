package attach

import (
	"errors"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/wiring"
)

// The one rule for which daemon potluck's commands use (Choose).
func TestChooseDaemon(t *testing.T) {
	prev := config.Wired()
	t.Cleanup(func() { config.Use(prev) })
	config.Use(config.Names{HomeEnvs: []string{"POTLUCK_HOME"}, HomeDirs: []string{".potluck"}, Sockets: []string{"potluck.sock"}})
	compute := append([]string{"core", "teamlink"}, wiring.ComputeModules...)
	supper := daemonrecord.Record{Program: "supper", Home: "/u/.supper", Socket: "/u/.supper/supper.sock", Modules: compute}
	other := daemonrecord.Record{Program: "other", Home: "/u/.other", Socket: "/u/.other/o.sock", Modules: compute}
	kitOnly := daemonrecord.Record{Program: "kitonly", Home: "/u/.kit", Socket: "/u/.kit/k.sock", Modules: []string{"core", "teamlink"}}
	own := daemonrecord.Record{Program: "potluck", Home: "/tmp/pl", Socket: "/tmp/pl/potluck.sock", Modules: compute}
	for _, tc := range []struct {
		name   string
		facts  Facts
		live   []daemonrecord.Record
		liveEr error
		want   string // the program attached to; "" potluck's own
		err    string
		noRead bool // the records are never read
	}{
		{name: "box credential", facts: Facts{BoxCredential: true, HomeEnv: "/u/.supper"}, live: []daemonrecord.Record{supper}, noRead: true},
		{name: "own home exists", facts: Facts{OwnHome: true}, live: []daemonrecord.Record{supper}, noRead: true},
		{name: "POTLUCK_HOME: a new home", facts: Facts{HomeEnv: "/u/new"}, live: []daemonrecord.Record{supper}},
		{name: "POTLUCK_HOME: another program's running home", facts: Facts{HomeEnv: "/u/.supper"}, live: []daemonrecord.Record{other, supper}, want: "supper"},
		{name: "POTLUCK_HOME: a home whose daemon serves no compute", facts: Facts{HomeEnv: "/u/.kit"}, live: []daemonrecord.Record{kitOnly}},
		{name: "one other compute daemon", live: []daemonrecord.Record{supper, kitOnly, own}, want: "supper"},
		{name: "none", live: []daemonrecord.Record{kitOnly, own}},
		{name: "no records", live: nil},
		{name: "two others", live: []daemonrecord.Record{supper, other}, err: "more than one Potluck daemon (supper's at /u/.supper, other's at /u/.other)"},
		{name: "records unreadable", liveEr: errors.New("permission denied"), err: "permission denied"},
		{name: "records unreadable, POTLUCK_HOME set", facts: Facts{HomeEnv: "/u/.supper"}, liveEr: errors.New("permission denied")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := false
			got, err := Choose(tc.facts, func(keep func(daemonrecord.Record) bool) ([]daemonrecord.Record, error) {
				read = true
				var out []daemonrecord.Record
				for _, r := range tc.live {
					if keep(r) {
						out = append(out, r)
					}
				}
				return out, tc.liveEr
			})
			switch {
			case tc.err != "":
				if err == nil || !strings.Contains(err.Error(), tc.err) || (strings.Contains(tc.err, "more than") && !strings.Contains(err.Error(), "POTLUCK_HOME=")) {
					t.Fatalf("err %v, want %q", err, tc.err)
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			name := ""
			if got != nil {
				name = got.Program
			}
			if name != tc.want || (tc.noRead && read) {
				t.Fatalf("attached to %q (records read: %v), want %q", name, read, tc.want)
			}
		})
	}
}
