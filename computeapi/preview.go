package computeapi

// PreviewRequest explicitly publishes a sandbox HTTP port. Public must be
// true; private access uses the authenticated port channel instead.
type PreviewRequest struct {
	Port   int    `json:"port"`
	Public bool   `json:"public"`
	Revoke string `json:"revoke,omitempty"`
}
type Preview struct {
	ID        string `json:"id"`
	Port      int    `json:"port"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}
type PreviewList struct {
	Previews []Preview `json:"previews"`
}
