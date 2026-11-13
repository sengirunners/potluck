package api

import "time"

// Owned by the tailnet builder. See SPEC.md "Remote access (tailnet)" and API.md "/v0/tailnet".

// TailnetStatus is GET /v0/tailnet.
type TailnetStatus struct {
	Enabled  bool   `json:"enabled"`
	Running  bool   `json:"running"`
	Hostname string `json:"hostname,omitempty"`
	Tailnet  string `json:"tailnet,omitempty"`   // MagicDNS suffix
	URL      string `json:"url,omitempty"`       // this daemon's tailnet base URL
	HTTPSURL string `json:"https_url,omitempty"` // MagicDNS HTTPS base URL, when listening
	// AuthURL is set while the node waits for a browser login.
	AuthURL string `json:"auth_url,omitempty"`
	Error   string `json:"error,omitempty"`
}

// TailnetUpRequest is POST /v0/tailnet/up.
type TailnetUpRequest struct {
	Hostname string `json:"hostname,omitempty"` // must be potluck-<name> or zm-<name>; default: config's tailnet.hostname, else zm-<machine> for a node made before the rename's step 2, else potluck-<machine>
	// AuthKey is used once to join and never saved; else the daemon's
	// TS_AUTHKEY, else an interactive login via AuthURL.
	AuthKey string `json:"auth_key,omitempty"`
	// ControlURL overrides the coordination server (e.g. Headscale).
	ControlURL string `json:"control_url,omitempty"`
}

// Peer is another node on the tailnet, named zm-<machine>, that answered
// GET /v0/health with this protocol major. Peers are listed only; v0
// forwards nothing between daemons. Offline peers keep their last sighting.
type Peer struct {
	Machine  string    `json:"machine"`
	URL      string    `json:"url"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"last_seen,omitzero"`
	Version  string    `json:"version,omitempty"`
	Owner    string    `json:"owner,omitempty"` // tailnet login that owns the node
}

// PeerList is GET /v0/peers.
type PeerList struct {
	Peers []Peer `json:"peers"`
}

// Tailnet event types (machine-level).
const (
	EventTailnetUp   = "dev.zm.tailnet.up"
	EventTailnetDown = "dev.zm.tailnet.down"
)
