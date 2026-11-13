package netguard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestBlocked(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "127.9.9.9", "10.0.0.1", "172.16.5.4", "192.168.1.1", "169.254.169.254", "100.64.0.1",
		"100.100.1.1", "0.0.0.0", "0.1.2.3", "224.0.0.1", "255.255.255.255", "198.18.0.1", "::1", "::", "fe80::1", "fd00:ec2::254",
		"fd7a:115c:a1e0::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::a9fe:a9fe", "2002:a9fe:a9fe::1",
		"2001:0:4136:e378::1", "ff02::1", "fe80::1%en0", "::7f00:1"} {
		if !Blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s not blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "140.82.112.3", "2606:4700:4700::1111", "::ffff:1.1.1.1"} {
		if Blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s blocked", s)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for _, u := range []string{"https://localhost./", "https://LOCALHOST/", "https://foo.localhost/", "https://printer.local/",
		"https://intranet/", "https://[::ffff:127.0.0.1]/", "https://0x7f.0.0.1/", "https://2130706433/"} {
		if _, err := CheckURL(u, false, "url"); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if _, err := CheckURL("https://hooks.example.com/zm?x=1#frag", false, "url"); err != nil {
		t.Errorf("public https refused: %v", err)
	}
	if _, err := CheckURL("http://127.0.0.1:9/", true, "url"); err != nil {
		t.Errorf("allow_private refused loopback: %v", err)
	}
}

// The dial check runs after DNS: a public-looking name that resolves to a
// private address is refused at connect time (rebinding), and a proxy in
// the environment is never used.
func TestDialRefusesPrivateAfterResolution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	t.Setenv("HTTPS_PROXY", srv.URL)
	t.Setenv("HTTP_PROXY", srv.URL)
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	c := Client(2*time.Second, false)
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		resp, err := c.Get("http://" + host + port + "/")
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s: dialled a loopback address", host)
		}
		if !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("%s: %v, want the blocked-address refusal", host, err)
		}
	}
	// The allow-private client reaches it.
	resp, err := Client(2*time.Second, true).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
