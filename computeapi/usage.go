package computeapi

// BoxUsage is one ended box's usage row (GET /v0/usage; SPEC 20 "Boxes").
// BillableMinutes is ceil(min(lease, driver minutes)); Billed is false for a
// reclaimed or lost box. No price: the team pool is free (12.5.8 adds rates).
type BoxUsage struct {
	Handle          string   `json:"handle"`
	Target          string   `json:"target"`
	Area            string   `json:"area,omitempty"`
	Actor           string   `json:"actor"`
	Size            string   `json:"size,omitempty"`
	ImageDigest     string   `json:"image_digest,omitempty"`
	EndState        string   `json:"end_state"`
	Acquired        string   `json:"acquired,omitempty"`
	Ended           string   `json:"ended"`
	LeaseS          int      `json:"lease_s"`
	DriverUnit      string   `json:"driver_unit,omitempty"`
	DriverAmount    *float64 `json:"driver_amount,omitempty"`
	BillableMinutes int      `json:"billable_minutes"`
	Billed          bool     `json:"billed"`
}

// UsageList is GET /v0/usage: the newest 1000 rows the caller may read.
// BilledMinutes counts every billed row that matches, not only those listed;
// Truncated says more rows matched than were listed.
type UsageList struct {
	Project       string     `json:"project"`
	Boxes         []BoxUsage `json:"boxes"`
	BilledMinutes int        `json:"billed_minutes"`
	Truncated     bool       `json:"truncated,omitempty"`
}

// AreaMinuteLimit caps the sandbox minutes one area of a project may use per
// calendar period (UTC): once its billed minutes for the period, counting its
// live sandboxes so far, reach Minutes, a new sandbox there is refused. A
// sandbox already running is not stopped. Kept in project.toml as
// [[box_minutes]] (the usage module's section).
type AreaMinuteLimit struct {
	Area    string `json:"area"`
	Minutes int    `json:"minutes"`
	Period  string `json:"period"` // day | week | month
}

// AreaMinuteLimits is GET /v0/usage/limits: the project's per-area limits.
type AreaMinuteLimits struct {
	Project string            `json:"project"`
	Limits  []AreaMinuteLimit `json:"limits"`
}

// SetAreaMinuteLimitRequest is PUT /v0/usage/limits/{area}; Minutes 0
// removes the area's limit.
type SetAreaMinuteLimitRequest struct {
	Minutes int    `json:"minutes"`
	Period  string `json:"period,omitempty"` // day | week | month; default month
}
