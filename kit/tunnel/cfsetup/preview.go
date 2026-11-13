package cfsetup

import (
	"context"
	"fmt"
	"io"
)

func (s *Setup) checkPreviewDomain() error {
	if s.PreviewDomain == "" {
		return nil
	}
	domain, err := CheckHostname(s.PreviewDomain)
	if err != nil {
		return err
	}
	if len(domain) > 204 {
		return fmt.Errorf("preview domain exceeds 204 characters")
	}
	s.PreviewDomain = domain
	return nil
}
func (s *Setup) routePreview(ctx context.Context, st State) error {
	if st.PreviewDomain == "" {
		return nil
	}
	if _, err := s.CF.Run(ctx, s.env(), io.Discard, append(s.origin(), "route", "dns", st.TunnelID, "*."+st.PreviewDomain)...); err != nil {
		return fmt.Errorf("routing preview wildcard: %w", err)
	}
	return nil
}

// Exact authority/broker host rules must precede this wildcard, so the API
// listener still receives its edge secret and never becomes a preview host.
func (s *Setup) previewIngress(st State) string {
	if st.PreviewDomain == "" {
		return ""
	}
	return fmt.Sprintf("  - hostname: %s\n    service: http://%s\n", yamlQuote("*."+st.PreviewDomain), s.Listen)
}
