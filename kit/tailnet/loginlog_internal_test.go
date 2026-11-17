package tailnet

import (
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// tsnet's own format for the line (tsnet.Server.printAuthURLLoop).
const tsnetLoginFormat = "To start this tsnet server, restart with TS_AUTHKEY set, or go to: %s"

func TestQuietLoginLogsEachURLOnceThenBacksOff(t *testing.T) {
	clock := time.Unix(0, 0)
	var got []string
	logf := quietLogin(func(format string, args ...any) {
		got = append(got, fmt.Sprintf(format, args...))
	}, func() time.Time { return clock })

	urlA := "https://login.tailscale.com/a/aaa"
	urlB := "https://login.tailscale.com/a/bbb"
	login := func(url string) string { return fmt.Sprintf(tsnetLoginFormat, url) }

	// tsnet's loop: the same URL every 5 s for 3 hours.
	var want []string
	for elapsed := time.Duration(0); elapsed <= 3*time.Hour; elapsed += 5 * time.Second {
		clock = time.Unix(0, 0).Add(elapsed)
		logf(tsnetLoginFormat, urlA)
		// Logged at 0, then after 1 min, 10 min more, then hourly.
		switch elapsed {
		case 0, time.Minute, 11 * time.Minute, 71 * time.Minute, 131 * time.Minute:
			want = append(want, login(urlA))
		}
	}
	// Other lines are never held back.
	logf("AuthLoop: state is %v; done", "Running")
	logf("AuthLoop: state is %v; done", "Running")
	want = append(want, "AuthLoop: state is Running; done", "AuthLoop: state is Running; done")
	// A new URL logs at once and starts the gaps again.
	logf(tsnetLoginFormat, urlB)
	clock = clock.Add(30 * time.Second)
	logf(tsnetLoginFormat, urlB)
	clock = clock.Add(30 * time.Second)
	logf(tsnetLoginFormat, urlB)
	want = append(want, login(urlB), login(urlB))

	if len(got) != len(want) {
		t.Fatalf("logged %d lines, want %d:\n%q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestQuietLoginNilUsesQuietStandardLogger(t *testing.T) {
	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	logf := quietLogin(nil, func() time.Time { return time.Unix(0, 0) })
	if logf == nil {
		t.Fatal("a nil logf must become log.Printf, else tsnet logs the URL every 5 s itself")
	}
	logf(tsnetLoginFormat, "https://login.tailscale.com/a/aaa")
	logf(tsnetLoginFormat, "https://login.tailscale.com/a/aaa")
	if n := strings.Count(buf.String(), "To start this tsnet server"); n != 1 {
		t.Fatalf("standard logger got the login line %d times, want 1:\n%s", n, buf.String())
	}
}
