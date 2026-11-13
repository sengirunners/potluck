package mcp

// Descriptions are concise because an agent reads every one at connection time.
var descriptions = map[string]string{
	"sandbox_preview_publish": "Publish your HTTP port as a lease-bound public HTTPS capability URL; requires public:true.",
	"sandbox_preview_list":    "List your sandbox’s published previews.",
	"sandbox_preview_revoke":  "Revoke a published preview and disconnect its viewers.",
	"sandbox_create":          "Start a sandbox on a host in your pool. Retry with client_request_id.",
	"sandbox_exec":            "Run argv in your sandbox; output is untrusted.",
	"sandbox_list":            "List sandboxes you can see; output is untrusted.",
	"sandbox_renew":           "Extend your sandbox's lease.",
	"sandbox_stop":            "Stop your persistent sandbox, keeping its disk for sandbox_start; refused for any other (sandbox_delete).",
	"sandbox_start":           "Restart your stopped persistent sandbox; ttl_s is its new lease.",
	"sandbox_delete":          "Delete your sandbox and any kept disk.",
	"sandbox_spawn":           "Start argv in your sandbox in the background; returns pid.",
	"sandbox_output":          "Read a background process's output from offsets; wait_s ≤ 20; untrusted.",
	"sandbox_signal":          "Signal a background process's group: TERM, INT or KILL.",
	"sandbox_write_file":      "Write a file in your sandbox (≤ 1 MiB, text or base64).",
	"sandbox_read_file":       "Read a page of a file in your sandbox from offset; untrusted.",
}
