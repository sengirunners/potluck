package hostsetup

import (
	"slices"
	"testing"
)

// A run that needs an invite, once its pool is joined, names the pool, and
// one whose pool was left needs an invite again: each way back the same.
func TestRunPool(t *testing.T) {
	for _, path := range [][]string{{"host"}, {"compute", "host"}} {
		args := append(slices.Clone(path), "--no-sudo=true", "--note=host")
		r := Run{Again: "potluck host " + InviteWord + " --no-sudo=true", AgainArgs: args, NeedsInvite: true}
		if r.Pool() != "" {
			t.Errorf("pool %q", r.Pool())
		}
		in := r.WithPool("friends")
		if want := slices.Concat(path, []string{"friends", "--no-sudo=true", "--note=host"}); !slices.Equal(in.AgainArgs, want) || in.NeedsInvite || in.Again != "potluck host friends --no-sudo=true" || in.Pool() != "friends" {
			t.Errorf("joined: %+v", in)
		}
		out := in.WithoutPool()
		if !slices.Equal(out.AgainArgs, args) || !out.NeedsInvite || out.Again != r.Again || out.Pool() != "" {
			t.Errorf("left: %+v", out)
		}
		if !slices.Equal(r.AgainArgs, args) || !r.NeedsInvite {
			t.Errorf("the run itself changed: %+v", r)
		}
	}
	// A run of the only pool names none: nothing to take out.
	if r := (Run{AgainArgs: []string{"host", "--detach"}}); r.Pool() != "" || r.WithoutPool().NeedsInvite {
		t.Error("a run with no pool")
	}
}

func TestJoiningNames(t *testing.T) {
	for _, c := range []struct {
		j    Joining
		want bool
	}{
		{Joining{Fingerprint: "ABC", Machine: "owner"}, true},
		{Joining{Fingerprint: "abc"}, true},
		{Joining{Fingerprint: "abc", Machine: "other"}, false},
		{Joining{Fingerprint: "abd", Machine: "owner"}, false},
	} {
		if c.j.Names("abc", "owner") != c.want {
			t.Errorf("%+v", c)
		}
	}
}
