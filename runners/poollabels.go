package runners

// LabelAsk is what a job's runs-on labels ask of the pool.
type LabelAsk struct {
	// Ours: the labels name the pool's label, plain or <label>:<size>. Whether
	// any box can serve them now is not part of it.
	Ours bool
	// Why, when Ours, is why the pool refuses the labels whatever boxes exist
	// (a label it does not serve, an unnameable size). Empty when it accepts.
	Why string
	// Size, OS and Image are what the labels ask of a box. Image is the
	// published named-image label (such as xcode), if any. When Ours and Why
	// is empty, the setup's default size and linux are filled in as placement
	// does; with a Why they are only what the labels named.
	Size, OS, Image string
}

// ParseLabels reads a job's runs-on labels the way the broker does for a
// workflow_job delivery, against the pool's setup (GET /v0/runners/pool):
// the same rules, so a caller that only needs to know whether a queued job
// targets the pool, independent of which hosts are up, asks here instead of
// keeping its own copy. Only Setup's Label and DefaultSize are read.
func ParseLabels(labels []string, setup Setup) LabelAsk {
	w, ours, why := parseLabels(labels, setup.Label, setup.DefaultSize)
	if !ours {
		return LabelAsk{}
	}
	return LabelAsk{Ours: true, Why: why, Size: w.size, OS: w.os, Image: w.image}
}
