package github_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
			drv.LookupToken = func(context.Context, string) (string, error) { return tok, nil }
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

// GitHub's rate-limit answers are told apart from other refusals, with the
// reset the driver can read.
func TestRateLimitedLookups(t *testing.T) {
	gh := githubfake.New(t)
	gh.AddUser("ana", "User")
	drv := github.New(gh.URL, nil)
	reset := time.Now().Add(3 * time.Second)
	gh.RateLimitUntil(reset)

	var rl *owners.RateLimited
	if _, err := drv.ByLogin(context.Background(), "ana"); !errors.As(err, &rl) {
		t.Fatalf("by login while limited: %v, want *owners.RateLimited", err)
	}
	if d := rl.ResetAt.Sub(reset); d < -time.Second || d > 2*time.Second {
		t.Fatalf("reset %v, want about %v", rl.ResetAt, reset)
	}
	if _, err := drv.ByID(context.Background(), "github:1"); !errors.As(err, &rl) {
		t.Fatalf("by id while limited: %v, want *owners.RateLimited", err)
	}
	if _, err := drv.OrgMember(context.Background(), "o", "ana"); errors.Is(err, owners.ErrNoView) || !errors.As(err, &rl) {
		// no lookup token: ErrNoView comes first; the limit needs a token
		drv.LookupToken = func(context.Context, string) (string, error) { return "tok", nil }
		if _, err := drv.OrgMember(context.Background(), "o", "ana"); !errors.As(err, &rl) {
			t.Fatalf("org member while limited: %v, want *owners.RateLimited", err)
		}
	}
	gh.RateLimitUntil(time.Time{})
	if a, err := drv.ByLogin(context.Background(), "ana"); err != nil || a.Login != "ana" {
		t.Fatalf("after the limit: %+v, %v", a, err)
	}
}

// The rate limit is read from 429, from a Retry-After in seconds or as a
// date, and only from a 403 that says so: any other 403 is not one.
func TestRateLimitAnswers(t *testing.T) {
	date := time.Now().Add(90 * time.Second).UTC().Truncate(time.Second)
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		limited bool
		reset   time.Time // zero: not checked
	}{
		{"429", 429, nil, true, time.Time{}},
		{"403 retry-after seconds", 403, map[string]string{"Retry-After": "60"}, true, time.Now().Add(60 * time.Second)},
		{"403 retry-after date", 403, map[string]string{"Retry-After": date.Format(http.TimeFormat)}, true, date},
		{"403 none remaining", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(date.Unix(), 10)}, true, date},
		{"403 calls remaining", 403, map[string]string{"X-RateLimit-Remaining": "12"}, false, time.Time{}},
		{"403 plain", 403, nil, false, time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range c.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(c.status)
			}))
			defer srv.Close()
			_, err := github.New(srv.URL, nil).ByLogin(context.Background(), "ana")
			var rl *owners.RateLimited
			if got := errors.As(err, &rl); got != c.limited {
				t.Fatalf("limited = %v (%v), want %v", got, err, c.limited)
			}
			if c.limited && !c.reset.IsZero() {
				if d := rl.ResetAt.Sub(c.reset); d < -2*time.Second || d > 2*time.Second {
					t.Fatalf("reset %v, want about %v", rl.ResetAt, c.reset)
				}
			}
		})
	}
}
