package runners_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// ParseLabels must agree with the broker: a workflow_job delivery is recorded
// exactly when ParseLabels says the labels are the pool's, and a refusal it
// reports is the reason the broker records.
func TestParseLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   runners.LabelAsk
		def    string
	}{
		{name: "plain", labels: []string{"MyPool"}, want: runners.LabelAsk{Ours: true, OS: "linux"}},
		{name: "default size", labels: []string{"mypool"}, def: "c1m2", want: runners.LabelAsk{Ours: true, Size: "c1m2", OS: "linux"}},
		{name: "sized", labels: []string{"mypool:c2m4"}, def: "c1m2", want: runners.LabelAsk{Ours: true, Size: "c2m4", OS: "linux"}},
		{name: "self-hosted and arch", labels: []string{"self-hosted", "MYPOOL:c1m2", "ARM64"}, want: runners.LabelAsk{Ours: true, Size: "c1m2", OS: "linux"}},
		{name: "macos", labels: []string{"self-hosted", "mypool", "macOS", "ARM64"}, want: runners.LabelAsk{Ours: true, OS: "macos"}},
		{name: "windows", labels: []string{"mypool", "windows"}, want: runners.LabelAsk{Ours: true, OS: "windows"}},
		{name: "xcode image", labels: []string{"mypool", "macos", "xcode"}, want: runners.LabelAsk{Ours: true, OS: "macos", Image: "xcode"}},
		{name: "other runner", labels: []string{"self-hosted", "sengi"}},
		{name: "github hosted", labels: []string{"ubuntu-latest"}},
		{name: "no labels"},
		{name: "pool prefix only", labels: []string{"mypoolish"}},
		{name: "extra label", labels: []string{"mypool", "gpu"}, want: runners.LabelAsk{Ours: true, Why: "it asks for labels the pool does not serve: gpu"}},
		{name: "two sizes", labels: []string{"mypool:c1m2", "mypool:c2m4"}, want: runners.LabelAsk{Ours: true, Size: "c1m2", Why: "it asks for labels the pool does not serve: mypool:c2m4"}},
		{name: "two os", labels: []string{"mypool", "macos", "windows"}, want: runners.LabelAsk{Ours: true, OS: "macos", Why: "it asks for labels the pool does not serve: windows"}},
		{name: "bad size", labels: []string{"mypool:-x"}, want: runners.LabelAsk{Ours: true, Size: "-x", Why: `no size "-x" in the pool`}},
	}
	ctx := context.Background()
	for i, tt := range tests {
		job := int64(9000 + i)
		t.Run(tt.name, func(t *testing.T) {
			// One broker per case: a job no box fits stays waiting and undone.
			f := newPoolFixture(t, func(c *config.Runners) { c.Label = "MyPool" })
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			setup := runners.Setup{Label: "MyPool", DefaultSize: tt.def}
			got := runners.ParseLabels(tt.labels, setup)
			if got != tt.want {
				t.Fatalf("ParseLabels(%v) = %+v, want %+v", tt.labels, got, tt.want)
			}
			// The broker's own path: a queued delivery is recorded (with its
			// refusal) when the labels are the pool's, and not at all when not.
			if code := f.deliver(t, "workflow_job", jobEvent("queued", job, 900+int64(i), "acme/app", true, tt.labels...)); code != http.StatusAccepted {
				t.Fatalf("deliver: %d", code)
			}
			eventually(t, "delivery applied", func() bool {
				_, recorded := f.jobs(t)[job]
				return recorded || (!got.Ours && f.svc.Undone(ctx) == 0)
			})
			j, recorded := f.jobs(t)[job]
			if recorded != got.Ours {
				t.Fatalf("broker recorded=%v for %v, ParseLabels ours=%v", recorded, tt.labels, got.Ours)
			}
			if got.Ours && got.Why != "" && (j.State != "ignored" || j.Why != got.Why) {
				t.Fatalf("broker recorded %q %q, ParseLabels why %q", j.State, j.Why, got.Why)
			}
		})
	}
}
