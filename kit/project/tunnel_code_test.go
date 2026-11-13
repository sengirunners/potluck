package project

import (
	"strings"
	"testing"
)

func TestTunnelJoinCode(t *testing.T) {
	secret := NewTunnelSecret()
	fp := strings.Repeat("a", KeyFingerprintLen)
	c, err := ParseJoinCode("https://zm.example.com/#" + fp + ":" + strings.ToLower(secret))
	if err != nil || c.URL != "https://zm.example.com" || c.Fingerprint != fp || c.Secret != NormalizeJoinSecret(secret) || c.Machine != "" {
		t.Fatalf("parse: %+v %v", c, err)
	}
	for _, bad := range []string{
		"http://zm.example.com#" + fp + ":" + secret,       // http off loopback
		"https://zm.example.com/zm#" + fp + ":" + secret,   // a path
		"https://zm.example.com#" + fp[:16] + ":" + secret, // a node fingerprint
		"https://zm.example.com#" + fp + ":ABCD-EFGH",      // a 40-bit secret
		"https://zm.example.com?x=1#" + fp + ":" + secret,
		"ftp://zm.example.com#" + fp + ":" + secret,
	} {
		if _, err := ParseJoinCode(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := ParseJoinCode("http://127.0.0.1:7435#" + fp + ":" + secret); err != nil {
		t.Errorf("loopback http: %v", err)
	}
	// A tailnet code is unchanged and never takes a tunnel secret.
	if _, err := ParseJoinCode("zm-mini:" + fp[:16] + ":" + secret); err == nil {
		t.Error("tailnet code with a 125-bit secret")
	}
	// The proof binds the key and the machine.
	p := RedeemProof(secret, fp, "zmkey:a", "m")
	if p == RedeemProof(secret, fp, "zmkey:b", "m") || p == RedeemProof(secret, fp, "zmkey:a", "n") || p != RedeemProof(strings.ToLower(secret), fp, "zmkey:a", "m") {
		t.Fatal("proof not bound to key and machine")
	}
}
