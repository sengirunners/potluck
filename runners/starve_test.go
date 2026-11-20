package runners_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// Serves waiting for a size no box has, nudged over and over (boxes freed
// often), never keep a new queued job that fits a free box from its first
// try: the one serve worker takes never-tried deliveries first (h77nah).
func TestWaitingServesDoNotStarveANewJob(t *testing.T) {
	f := newPoolFixtureStarting(t, func(*config.Runners) {}, func(s *runners.Service) { s.Serves = 1 },
		box("a", "c1m2", "linux", "1"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	ctx := context.Background()
	const waiting = 4
	for i := range int64(waiting) {
		f.deliver(t, "workflow_job", jobEvent("queued", 8501+i, 180+i, "acme/app", true, "pool:c4m16"))
	}
	waitFor(t, "the first big job waiting", time.Now().Add(20*time.Second), func() bool {
		return strings.Contains(f.jobs(t)[8501].Why, "c4m16")
	})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				f.svc.Nudge(ctx)
				time.Sleep(time.Millisecond)
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	f.deliver(t, "workflow_job", jobEvent("queued", 8600, 200, "acme/app", true, "pool:c1m2"))
	waitFor(t, "the new job served on the free box", time.Now().Add(15*time.Second), func() bool {
		return f.liveOn(t, "a") == 1 && f.jobs(t)[8600].State == "serving"
	})
	// Every big job had its first try too, while the others kept waiting.
	waitFor(t, "every big job tried", time.Now().Add(15*time.Second), func() bool {
		js := f.jobs(t)
		for i := range int64(waiting) {
			if !strings.Contains(js[8501+i].Why, "c4m16") {
				return false
			}
		}
		return true
	})
}
