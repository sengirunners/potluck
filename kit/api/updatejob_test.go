package api_test

import (
	"encoding/json"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// A build's update job round-trips three distinct ways: no job, a job whose
// min-age is unknown, and a job that takes a release at once (0, never
// confused with unknown).
func TestUpdateJobRoundTrips(t *testing.T) {
	zero := int64(0)
	day := int64(86400)
	for _, c := range []struct {
		name string
		job  *api.UpdateJob
		wire string
	}{
		{"no job", nil, `{"version":"v","commit":"c","started":"0001-01-01T00:00:00Z"}`},
		{"min-age unknown", &api.UpdateJob{}, `{"version":"v","commit":"c","started":"0001-01-01T00:00:00Z","job":{}}`},
		{"min-age 0", &api.UpdateJob{MinAgeS: &zero}, `{"version":"v","commit":"c","started":"0001-01-01T00:00:00Z","job":{"min_age_s":0}}`},
		{"min-age a day", &api.UpdateJob{MinAgeS: &day}, `{"version":"v","commit":"c","started":"0001-01-01T00:00:00Z","job":{"min_age_s":86400}}`},
	} {
		b, err := json.Marshal(api.MachineStats{Potluck: &api.MachineZm{Version: "v", Commit: "c", Job: c.job}})
		if err != nil {
			t.Fatal(err)
		}
		var st api.MachineStats
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
		var raw struct {
			Potluck json.RawMessage `json:"potluck"`
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatal(err)
		}
		if string(raw.Potluck) != c.wire {
			t.Errorf("%s: sent %s, want %s", c.name, raw.Potluck, c.wire)
		}
		got := st.Build().Job
		switch {
		case c.job == nil:
			if got != nil {
				t.Errorf("%s: read back %+v", c.name, got)
			}
		case got == nil:
			t.Errorf("%s: job lost", c.name)
		case c.job.MinAgeS == nil:
			if got.MinAgeS != nil {
				t.Errorf("%s: min-age read back as %d", c.name, *got.MinAgeS)
			}
		case got.MinAgeS == nil || *got.MinAgeS != *c.job.MinAgeS:
			t.Errorf("%s: min-age read back as %v", c.name, got.MinAgeS)
		}
	}
}
