package teamlink

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// From the network only a member machine's node-bound token presses, and
// only for an owner of every team project, of which the home must hold one:
// a node-bound token that is also limited never passes (the router stops
// one first; this check does not lean on it), and on a parked daemon no
// network caller passes, whatever it owns.
func TestPressedByFromTheNetwork(t *testing.T) {
	_, d := hooksService(t)
	d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	judy := d.Projects.LocalIdentity()
	orchid, ok := d.Projects.Lookup("orchid")
	if !ok {
		t.Fatal("no orchid")
	}
	member := func(limit string) *http.Request {
		sub := &authz.Subject{Identity: judy, Address: "human:" + judy.Name, Via: authz.ViaToken, Remote: true,
			Node: "nLAPTOP", Machine: "laptop", Scope: authz.All()}
		if limit != "" {
			sub.Cap = &authz.Cap{Project: orchid.ID, Role: authz.Owner, Limit: limit}
		}
		r := httptest.NewRequest(http.MethodPost, api.StandbyPath, nil)
		return r.WithContext(core.WithSubject(ctx, sub))
	}
	refused := func(name string, err error) {
		t.Helper()
		if api.HQRefusalReason(err) != api.ReasonNotOwner {
			t.Errorf("%s: %v, want not_owner", name, err)
		}
	}

	// orchid is solo: the home holds no team project.
	_, err := pressedBy(member(""), d, true)
	refused("an owner of a home with no team project", err)

	if err := d.Projects.SetTeam(ctx, orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	by, err := pressedBy(member(""), d, true)
	if err != nil || by.Identity != judy.Name || by.Machine != "laptop" || by.At.IsZero() {
		t.Fatalf("an owner of every team project: %+v %v", by, err)
	}
	_, err = pressedBy(member("answer"), d, true)
	refused("a limited node-bound token", err)

	d.Parked = true
	_, err = pressedBy(member(""), d, true)
	refused("an owner from the network on a parked daemon", err)
}
