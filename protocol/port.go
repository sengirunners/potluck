package protocol

// VerbPort moves bounded TCP pages through the ordinary lease and relay path.
// A driver only connects to 127.0.0.1 inside the named sandbox.
const VerbPort = "port"
const PortPage = 64 << 10

type PortRequest struct {
	Op     string `json:"op"`
	Port   int    `json:"port,omitempty"`
	Stream string `json:"stream,omitempty"`
	Data   []byte `json:"data,omitempty"`
}
type PortResult struct {
	Stream string `json:"stream,omitempty"`
	Data   []byte `json:"data,omitempty"`
	EOF    bool   `json:"eof,omitempty"`
}
