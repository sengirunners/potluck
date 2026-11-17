package localdriver

import (
	"maps"
	"testing"
)

func TestTTLConfigLimits(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	for _, key := range []string{"ttl_max_s", "default_ttl_s"} {
		for _, value := range []string{"abc", "0", "-5", "1.5", "604801"} {
			t.Run(key+"="+value, func(t *testing.T) {
				cfg := maps.Clone(f.cfg)
				cfg[key] = value
				if got := code(f.call(t, "describe", "", nil, cfg)); got != "bad_request" {
					t.Errorf("describe code = %q, want bad_request", got)
				}
			})
		}
	}

	cfg := maps.Clone(f.cfg)
	cfg["ttl_max_s"], cfg["default_ttl_s"] = "45", "60"
	meta := result(t, f.call(t, "describe", "", nil, cfg))
	if got := meta["ttl_max_s"]; got != float64(45) {
		t.Errorf("describe ttl_max_s = %v, want 45", got)
	}
	args := map[string]any{"request_id": "ttl-default", "actor": "a", "ref": "main", "commit": f.repo.Commit, "script": "probe.sh"}
	run := result(t, f.call(t, "acquire", "", args, cfg))
	if got := run["ttl_s"]; got != float64(45) {
		t.Errorf("acquire ttl_s = %v, want capped default 45", got)
	}
	args["request_id"], args["ttl_s"] = "ttl-too-high", 46
	if got := code(f.call(t, "acquire", "", args, cfg)); got != "bad_request" {
		t.Errorf("acquire code = %q, want bad_request", got)
	}
}
