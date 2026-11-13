package cfsetup

import (
	"strings"
	"testing"
)

func TestPreviewWildcardPreservesHostAndAuthorityRule(t *testing.T) {
	s := &Setup{ZmHome: t.TempDir(), Listen: "127.0.0.1:7435"}
	st := State{Hostname: "api.example.com", PreviewDomain: "example.com", TunnelID: "owned", EdgeSecret: "abcd"}
	config := s.cloudflaredConfig(st)
	apiAt := strings.Index(config, "hostname: api.example.com")
	previewAt := strings.Index(config, "hostname: '*.example.com'")
	fallback := strings.Index(config, "service: http_status:404")
	if apiAt < 0 || previewAt <= apiAt || fallback <= previewAt {
		t.Fatal(config)
	}
	if strings.Contains(config[previewAt:fallback], "httpHostHeader") {
		t.Fatal("preview Host replaced by API edge secret")
	}
	s.PreviewDomain = "example.com/path"
	if s.checkPreviewDomain() == nil {
		t.Fatal("URL path accepted as DNS domain")
	}
}
