package github_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/owners"
	"github.com/sengirunners/potluck/kit/owners/conformance"
	"github.com/sengirunners/potluck/kit/owners/github"
	"github.com/sengirunners/potluck/kit/owners/githubfake"
)

// idOf reads the fake's user id out of its token ("gho_fake<n>x<id>").
func idOf(t *testing.T, tok string) int64 {
	_, s, _ := strings.Cut(strings.TrimPrefix(tok, "gho_fake"), "x")
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("token %q: %v", tok, err)
	}
	return id
}

func TestConformance(t *testing.T) {
	gh := githubfake.New(t)
	drv := github.New(gh.URL, nil)
	conformance.Run(t, conformance.Harness{
		Accounts: drv,
		SetLookup: func(t *testing.T, tok string) {
			drv.LookupToken = func(context.Context) (string, error) { return tok, nil }
		},
		NewUser: func(t *testing.T, login string) string {
			_, tok := gh.AddUser(login, "User")
			return tok
		},
		SecondToken: func(t *testing.T, tok string) string { return gh.Token(idOf(t, tok)) },
		Rename:      func(t *testing.T, tok, login string) { gh.Rename(idOf(t, tok), login) },
		Revoke:      func(t *testing.T, tok string) { gh.Revoke(tok) },
		NewOrg:      func(t *testing.T, login string) { gh.AddUser(login, "Organization") },
		Suspend:     func(t *testing.T, tok string, gone bool) { gh.Gone(idOf(t, tok), gone) },
		MakeAdmin: func(t *testing.T, org, tok string) {
			o, err := github.New(gh.URL, nil).ByLogin(context.Background(), org)
			if err != nil {
				t.Fatal(err)
			}
			oid, _ := strconv.ParseInt(strings.TrimPrefix(o.ID, github.IDPrefix), 10, 64)
			gh.MakeAdmin(oid, idOf(t, tok))
		},
		Join: func(t *testing.T, org, tok string, active bool) {
			o, err := github.New(gh.URL, nil).ByLogin(context.Background(), org)
			if err != nil {
				t.Fatal(err)
			}
			state := "pending"
			if active {
				state = "active"
			}
			oid, _ := strconv.ParseInt(strings.TrimPrefix(o.ID, github.IDPrefix), 10, 64)
			gh.AddMember(oid, idOf(t, tok), state)
		},
		Device:  github.NewDevice(gh.URL, gh.ClientID, nil),
		Approve: func(t *testing.T, tok string, pending int) { gh.Approver(idOf(t, tok), pending) },
		Decline: func(t *testing.T) { gh.Approver(0, 0) },
		Expire:  func(t *testing.T) { gh.Approver(-1, 0) },
	})
}

func TestVerifyKindsAndOutage(t *testing.T) {
	gh := githubfake.New(t)
	d := github.New(gh.URL, nil)
	ctx := context.Background()
	u, tok := gh.AddUser("acme", "Organization")
	a, err := d.Verify(ctx, tok)
	if err != nil || a.Kind != owners.KindOrg || a.ID != "github:"+strconv.FormatInt(u.ID, 10) {
		t.Fatalf("org: %+v %v", a, err)
	}
	_, tok = gh.AddUser("dependabot", "Bot")
	if a, _ := d.Verify(ctx, tok); a.Kind != owners.KindBot {
		t.Fatalf("bot kind %q", a.Kind)
	}
	gh.Close()
	_, err = d.Verify(ctx, tok)
	if err == nil || errors.Is(err, owners.ErrBadToken) {
		t.Fatalf("GitHub down: %v, want an outage error (not a bad token)", err)
	}
	if strings.Contains(err.Error(), tok) {
		t.Fatalf("the error names the token: %v", err)
	}
}

func TestDeviceWrongClient(t *testing.T) {
	gh := githubfake.New(t)
	if _, err := github.NewDevice(gh.URL, "wrong-client", nil).Start(context.Background()); err == nil {
		t.Fatal("a wrong client id started a device flow")
	}
}

// GitHub's slow_down adds 5 seconds; its usual interval is 5 seconds.
func TestDeviceSlowDownAndInterval(t *testing.T) {
	for name, tc := range map[string]struct{ interval, slow, min int }{"slow_down": {1, 1, 6}, "interval 5": {5, 0, 5}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			gh := githubfake.New(t)
			gh.Interval, gh.SlowDown = tc.interval, tc.slow
			u, _ := gh.AddUser("ana", "User")
			gh.Approver(u.ID, 0)
			d := github.NewDevice(gh.URL, gh.ClientID, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			code, err := d.Start(ctx)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			tok, err := d.Wait(ctx, code)
			if err != nil || tok == "" {
				t.Fatalf("Wait: %q %v", tok, err)
			}
			if took := time.Since(start); took < time.Duration(tc.min)*time.Second {
				t.Fatalf("took %s, want at least %ds (GitHub's pacing ignored)", took, tc.min)
			}
		})
	}
}
