package e2bgateway

// TLS without a wildcard certificate: the gateway's own CA, name-constrained
// to its domain, signs one short-lived certificate per name asked for
// (api.<domain>, 49983-<id>.<domain>). One port serves TLS and plain HTTP.

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ca struct {
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	domain string
	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// systemBundles are the usual system CA bundle paths, first found wins.
var systemBundles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/cert.pem", "/etc/ssl/ca-bundle.pem"}

// loadCA reads <dir>/ca.pem and ca-key.pem (0600), making them on first use,
// and writes ca-bundle.pem: the system bundle plus this CA, for clients
// whose SSL_CERT_FILE replaces the system store.
func loadCA(dir, domain string) (*ca, error) {
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	c := &ca{domain: domain, leaves: map[string]*tls.Certificate{}}
	keyPEM, err := readSecretFile(keyPath)
	switch {
	case err == nil:
		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			return nil, err
		}
		if c.cert, c.key, err = parsePair(certPEM, []byte(keyPEM)); err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		if len(c.cert.PermittedDNSDomains) != 1 || c.cert.PermittedDNSDomains[0] != domain {
			return nil, fmt.Errorf("%s was made for domain %v, not %s (use another --state)", certPath, c.cert.PermittedDNSDomains, domain)
		}
		if time.Now().After(c.cert.NotAfter) {
			return nil, fmt.Errorf("%s expired; remove ca.pem and ca-key.pem to make a new one", certPath)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := c.create(certPath, keyPath); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return c, c.writeBundle(filepath.Join(dir, "ca-bundle.pem"))
}

func parsePair(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, errors.New("bad CA PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	return cert, key, err
}

func (c *ca) create(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "potluck-e2b local CA for " + c.domain},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		// Trusting this CA trusts it for the gateway's domain only.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{c.domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if c.cert, err = x509.ParseCertificate(der); err != nil {
		return err
	}
	c.key = key
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	kf, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer kf.Close()
	if err := pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func (c *ca) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

func (c *ca) writeBundle(path string) error {
	var sys []byte
	for _, p := range systemBundles {
		if b, err := os.ReadFile(p); err == nil {
			sys = b
			break
		}
	}
	if len(sys) > 0 && sys[len(sys)-1] != '\n' {
		sys = append(sys, '\n')
	}
	return os.WriteFile(path, append(sys, c.pem()...), 0o644)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

// leaf signs (or reuses) a certificate for name, which must be the domain or
// under it.
func (c *ca) leaf(name string) (*tls.Certificate, error) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if name != c.domain && !strings.HasSuffix(name, "."+c.domain) {
		return nil, fmt.Errorf("potluck-e2b serves %s and names under it, not %q", c.domain, name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.leaves[name]; l != nil && time.Until(l.Leaf.NotAfter) > time.Hour {
		return l, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	l := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key}
	if l.Leaf, err = x509.ParseCertificate(der); err != nil {
		return nil, err
	}
	if len(c.leaves) > 4096 { // names are per sandbox: keep the cache bounded
		clear(c.leaves)
	}
	c.leaves[name] = l
	return l, nil
}

// validDomain refuses a --domain that would make a useless or dangerous
// name constraint (an empty one permits every name): two or more labels of
// letters, digits and '-', none empty.
func validDomain(d string) error {
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return fmt.Errorf("--domain %q: want at least two labels (e2b.localhost)", d)
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return fmt.Errorf("--domain %q: bad label %q", d, l)
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("--domain %q: lower-case letters, digits and '-' only", d)
			}
		}
	}
	return nil
}

// checkAddr refuses plain HTTP on a non-loopback address: API keys and
// access tokens would cross the network in clear.
func checkAddr(host string, tlsOnly bool) error {
	if tlsOnly || host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("--addr %q is not loopback: add --tls-only so keys never cross the network in clear", host)
}

// tlsConfig is the listener's TLS: an operator's certificate, or the CA's
// per-name leaves.
func (g *gateway) tlsConfig(certFile, keyFile string) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
	switch {
	case certFile != "" || keyFile != "":
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		tc.Certificates = []tls.Certificate{pair}
	default:
		c, err := loadCA(g.cfg.StateDir, g.cfg.Domain)
		if err != nil {
			return nil, err
		}
		tc.GetCertificate = func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := h.ServerName
			if name == "" {
				name = c.domain
			}
			return c.leaf(name)
		}
	}
	return tc, nil
}

// sniffListener hands the server a *tls.Conn when the client's first byte
// is a TLS handshake (0x16), else the plain connection, so net/http still
// negotiates HTTP/2 by ALPN. Peeking happens per connection, off Accept.
type sniffListener struct {
	net.Listener
	tc    *tls.Config
	plain bool // serve plain HTTP too (loopback only: keys would cross in clear)
	conns chan net.Conn
	errc  chan error
}

func newSniffListener(l net.Listener, tc *tls.Config, plain bool) *sniffListener {
	s := &sniffListener{Listener: l, tc: tc, plain: plain, conns: make(chan net.Conn), errc: make(chan error, 1)}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				s.errc <- err
				return
			}
			go s.sniff(conn)
		}
	}()
	return s
}

func (s *sniffListener) sniff(conn net.Conn) {
	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	b, err := br.Peek(1)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return
	}
	var out net.Conn = &peekedConn{Conn: conn, r: br}
	switch {
	case b[0] == 0x16 && s.tc != nil:
		out = tls.Server(out, s.tc)
	case !s.plain:
		conn.Close()
		return
	}
	select {
	case s.conns <- out:
	case err := <-s.errc: // closed meanwhile
		s.errc <- err
		conn.Close()
	}
}

func (s *sniffListener) Accept() (net.Conn, error) {
	select {
	case c := <-s.conns:
		return c, nil
	case err := <-s.errc:
		s.errc <- err
		return nil, err
	}
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
