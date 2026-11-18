package core

import (
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
)

// TestOwnAddress: a caller's own records are those of its address, its
// identity, and for a replica's token its machine's sessions; on this
// machine's socket, this machine's human, machine and sessions. Nobody
// else's.
func TestOwnAddress(t *testing.T) {
	d := &Deps{Config: &config.Config{Machine: "auth", Human: "jo"}}
	friend := &authz.Subject{Identity: api.Identity{Name: "ana", Kind: api.IdentityHuman}, Address: "lap/abcd1234",
		Via: authz.ViaToken, Remote: true, Node: "n1", Machine: "lap"}
	local := &authz.Subject{Identity: api.Identity{Name: "jo", Kind: api.IdentityHuman}, Address: "human:jo", Via: authz.ViaLocal}
	plain := &authz.Subject{Identity: api.Identity{Name: "cy", Kind: api.IdentityHuman}, Address: "human:cy", Via: authz.ViaToken, Remote: true}
	squat := &authz.Subject{Identity: api.Identity{Name: "eve", Kind: api.IdentityHuman}, Address: "human:eve",
		Via: authz.ViaToken, Remote: true, Node: "n2", Machine: "auth"}
	for _, tc := range []struct {
		sub  *authz.Subject
		addr string
		want bool
	}{
		{friend, "lap/abcd1234", true},
		{friend, "lap/ffff0000", true}, // another of their sessions, ended or not
		{friend, "machine:lap", true},
		{friend, "human:ana", true},
		{friend, "human:bo", false},
		{friend, "other/abcd1234", false},
		{friend, "machine:other", false},
		{local, "auth/abcd1234", true},
		{local, "machine:auth", true},
		{local, "human:jo", true},
		{local, "lap/abcd1234", false},
		{local, "human:ana", false},
		{plain, "human:cy", true},
		{plain, "lap/abcd1234", false},
		{squat, "auth/abcd1234", false}, // a replica named as this machine gets none of its sessions
		{squat, "human:eve", true},
		{friend, "", false},
		{friend, "not an address", false},
		{nil, "human:ana", false},
	} {
		if got := d.OwnAddress(tc.sub, tc.addr); got != tc.want {
			who := "nil"
			if tc.sub != nil {
				who = tc.sub.Address
			}
			t.Errorf("OwnAddress(%s, %q) = %v, want %v", who, tc.addr, got, tc.want)
		}
	}
}
