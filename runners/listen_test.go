package runners

import (
	"context"
	"strings"
	"testing"
)

// runners.listen is loopback only: the proxy in front of public_url reaches
// it, nothing on the network does directly.
func TestListenIsLoopbackOnly(t *testing.T) {
	s := &Service{}
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.5:7440", "example.com:80", "nonsense"} {
		if err := s.listen(context.Background(), addr); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("listen %q: %v, want a loopback refusal", addr, err)
		}
	}
}
