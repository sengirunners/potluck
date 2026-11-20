package conformancecmd

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/sengirunners/potluck/conformance"
)

// ReportVersion is the shape of the --json-out file. A reader checks it before trusting the rest.
const ReportVersion = 1

// Report is what --json-out writes: one run, the results as they came, and the skipped checks listed apart, so a
// reader that only wants "what was not proven" does not filter the results itself.
type Report struct {
	Version int    `json:"version"`
	Driver  string `json:"driver"`
	Kind    string `json:"kind"`
	Strict  bool   `json:"strict"`
	// Passed is false when any check failed (exit 1); a skipped check is still a pass.
	Passed  bool                 `json:"passed"`
	Failed  int                  `json:"failed"`
	Skipped []SkippedCheck       `json:"skipped"`
	Results []conformance.Result `json:"results"`
}

// SkippedCheck names one check the run did not prove.
type SkippedCheck struct {
	Name  string `json:"name"`
	Group string `json:"group,omitempty"`
}

func newReport(driver, kind string, strict bool, res []conformance.Result) Report {
	r := Report{Version: ReportVersion, Driver: driver, Kind: kind, Strict: strict,
		Failed: conformance.Failed(res), Skipped: []SkippedCheck{}, Results: res}
	r.Passed = r.Failed == 0
	for _, s := range conformance.SkippedResults(res) {
		r.Skipped = append(r.Skipped, SkippedCheck{Name: s.Name, Group: s.Group})
	}
	return r
}

// writeReport writes r to path as indented JSON, through a temporary file in the same directory, so a reader never
// sees half a report.
func writeReport(path string, r Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".conformance-report-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
