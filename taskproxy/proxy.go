// Package taskproxy supplies a cooperative HTTP proxy for task processes.
// Clients can bypass it; it does not change a sandbox's network isolation.
package taskproxy

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Script runs a loopback proxy and a command in the caller's process group.
// It needs Python 3, and never reads the task's credential file.
//
//go:embed proxy.py
var Script string

// Options selects cooperative proxy behavior. The empty profile disables it.
type Options struct {
	Profile      string   `json:"profile"`
	DownloadMbit int      `json:"download_mbit"`
	AllowHosts   []string `json:"allow_hosts,omitempty"`
}

// DevelopmentHosts is the default list for Claude, GitHub, npm, Python and Go.
// A fresh slice keeps one task from changing another's defaults.
func DevelopmentHosts() []string {
	return []string{"api.anthropic.com", "github.com", "api.github.com", "uploads.github.com",
		"codeload.github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com",
		"github-releases.githubusercontent.com", "raw.githubusercontent.com", "*.actions.githubusercontent.com",
		"registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "proxy.golang.org",
		"sum.golang.org", "go.dev", "dl.google.com"}
}

var hostName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
var numericSuffix = regexp.MustCompile(`^(?:[0-9]+|0x[0-9a-f]+)$`)

// Config validates options and returns the helper's JSON configuration.
// Additional entries are DNS names or scoped subdomain wildcards. Wildcards
// over shared hosting spaces and names encoding arbitrary addresses are refused.
func (o Options) Config() ([]byte, error) {
	if o.Profile != "development" {
		return nil, fmt.Errorf("cooperative proxy profile must be development")
	}
	if o.DownloadMbit < 0 || o.DownloadMbit > 100000 {
		return nil, fmt.Errorf("proxy download rate must be 0..100000 Mbit/s (0 is unlimited)")
	}
	hosts := DevelopmentHosts()
	for _, raw := range o.AllowHosts {
		h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
		if !validEntry(h) {
			return nil, fmt.Errorf("proxy allow host %q must be a DNS name or scoped *.domain wildcard, not an address or shared hosting wildcard", raw)
		}
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return json.Marshal(struct {
		Hosts        []string `json:"hosts"`
		DownloadMbit int      `json:"download_mbit"`
	}{hosts, o.DownloadMbit})
}
