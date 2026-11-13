package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/config"
)

// oldCanonHeaders is canonHeaders before the rename's first step: only
// zm-* (and Content-Type, Authorization) signed.
func oldCanonHeaders(h http.Header, skip string, auth bool) string {
	var names []string
	for k := range h {
		l := strings.ToLower(k)
		if (l == "content-type" || strings.HasPrefix(l, "zm-") || (auth && l == "authorization")) && !strings.EqualFold(k, skip) {
			names = append(names, l)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		for _, v := range h.Values(n) {
			b.WriteString(strconv.Itoa(len(n)) + ":" + n + strconv.Itoa(len(v)) + ":" + v)
		}
	}
	s := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(s[:])
}

// For a request or answer that carries only Zm-* names (all a step 1 peer
// sends), the signed string is byte for byte what it was before the rename.
func TestSignedStringUnchangedForOldNames(t *testing.T) {
	hs := []http.Header{
		{},
		{"Zm-Key": {"k"}, "Zm-Date": {"1"}, "Zm-Nonce": {"n"}, "Zm-As": {"a", "b"}, "Content-Type": {"x"},
			"Authorization": {"Bearer zm_t"}, "Content-Length": {"3"}, "X-Zm-Edge-Secret": {"s"}, "Zm-Sig": {"sig"}},
		{"Zm-Home-Key": {"k"}, "Zm-Team": {"t"}, "Zm-Seal-Key": {"1.a.b"}, "Zm-Home-Sig": {"s"}, "Cache-Control": {"no-store"}},
	}
	for i, h := range hs {
		for _, auth := range []bool{true, false} {
			for _, skip := range []string{HeaderSig, HeaderHomeSig} {
				if canonHeaders(h, skip, auth) != oldCanonHeaders(h, skip, auth) {
					t.Fatalf("header set %d (auth %v, skip %s): canonical string changed", i, auth, skip)
				}
			}
		}
		old := []byte("zm-req/1\nfp\nPOST\n/v0/x\n" + h.Get(HeaderDate) + "\n" + h.Get(HeaderNonce) + "\n" + oldCanonHeaders(h, HeaderSig, true) + "\n" + sum([]byte("b")))
		if !bytes.Equal(requestString("fp", "POST", "/v0/x", h, []byte("b")), old) {
			t.Fatalf("header set %d: request string changed", i)
		}
	}
}

// signAs signs req as a member does, every header of the signed exchange
// under prefix: "Potluck-" as this build (rename step 2), "Zm-" as a step 1
// member (potluck 358a1e8c), which sent only the old names.
func signAs(t *testing.T, prefix string, req *http.Request, homeFP string, key ed25519.PrivateKey, body []byte) string {
	t.Helper()
	var nb [16]byte
	_, _ = rand.Read(nb[:])
	nonce := b64(nb[:])
	req.Header.Set(prefix+"Key", b64(key.Public().(ed25519.PublicKey)))
	req.Header.Set(prefix+"Date", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set(prefix+"Nonce", nonce)
	req.Header.Set(prefix+"Sig", b64(ed25519.Sign(key, requestString(homeFP, req.Method, req.URL.RequestURI(), req.Header, body))))
	return nonce
}

// A home of this build takes a request signed by a step 1 member (Zm-*
// names, a caller header Zm-As) and one signed under the new names; its
// answer carries the new names (Potluck-Home-Key, Potluck-Home-Sig) and
// verifies under the step 1 reading rules (VerifyAnswer: the new name
// first, then the old; reading is unchanged since step 1). An added
// Potluck-* or Zm-* header, or the nonce under both names, is refused.
func TestStepTwoDoorTakesBothMembers(t *testing.T) {
	h, srv, seen := home(t)
	fp, _ := h.Fingerprint()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	for _, prefix := range []string{"Zm-", "Potluck-"} {
		send := func(alter func(*http.Request)) (*http.Response, []byte, string) {
			t.Helper()
			body := []byte("hello")
			req, _ := http.NewRequest("POST", srv.URL+"/v0/x", bytes.NewReader(body))
			tok := "Bearer potluck_tok"
			if prefix == "Zm-" {
				tok = "Bearer zm_tok"
			}
			req.Header.Set("Authorization", tok)
			req.Header.Set(prefix+"As", "mini/abcd1234")
			nonce := signAs(t, prefix, req, fp, key, body)
			if alter != nil {
				alter(req)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp, b, nonce
		}
		seen.Store(nil)
		resp, b, nonce := send(nil)
		if resp.StatusCode != 201 || seen.Load() == nil || seen.Load().NodeID != KeyID(key.Public().(ed25519.PublicKey)) {
			t.Fatalf("%s member: %d %s %+v", prefix, resp.StatusCode, b, seen.Load())
		}
		if err := VerifyAnswer(fp, nonce, resp.StatusCode, resp.Header, b); err != nil {
			t.Fatalf("%s member, the answer: %v", prefix, err)
		}
		if resp.Header.Get("Potluck-Home-Sig") == "" || resp.Header.Get("Potluck-Home-Key") == "" ||
			resp.Header.Get("Zm-Home-Sig") != "" || resp.Header.Get("Zm-Home-Key") != "" {
			t.Fatalf("the home's answer headers: %v", resp.Header)
		}
		for name, alter := range map[string]func(*http.Request){
			"an unsigned Potluck-* header": func(r *http.Request) { r.Header.Set("Potluck-Project", "other") },
			"an unsigned Zm-* header":      func(r *http.Request) { r.Header.Set("Zm-Project", "other") },
			"the nonce under both names": func(r *http.Request) {
				r.Header.Set("Zm-Nonce", r.Header.Get(prefix+"Nonce"))
				r.Header.Set("Potluck-Nonce", r.Header.Get(prefix+"Nonce"))
			},
			"a wrong signature": func(r *http.Request) { r.Header.Set(prefix+"Sig", b64(make([]byte, 64))) },
		} {
			seen.Store(nil)
			if resp, b, _ := send(alter); resp.StatusCode != 401 || seen.Load() != nil {
				t.Errorf("%s member, %s: %d %s", prefix, name, resp.StatusCode, b)
			}
		}
	}
}

// A member of this build sends only the new names (a caller's Zm-As goes
// as it was put, signed; the transport's own headers are Potluck-*), and
// takes answers from a home of this build and from a step 1 home, whose
// answers carry the old names (rewritten here and signed again with the
// home's key, as a step 1 home signs).
func TestStepTwoMemberTakesBothHomes(t *testing.T) {
	h, srv, _ := home(t)
	fp, _ := h.Fingerprint()
	hk, err := h.key()
	if err != nil {
		t.Fatal(err)
	}
	stepOneHome := func(r *http.Response) error {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		r.Body.Close()
		for k, v := range r.Header {
			if rest, ok := strings.CutPrefix(k, "Potluck-"); ok {
				delete(r.Header, k)
				r.Header["Zm-"+rest] = v
			}
		}
		r.Header.Del("Zm-Home-Sig")
		sig := ed25519.Sign(hk, responseString(fp, get(r.Request.Header, HeaderNonce), r.StatusCode, r.Header, b))
		r.Header.Set("Zm-Home-Sig", b64(sig))
		r.Body = io.NopCloser(bytes.NewReader(b))
		return nil
	}
	var c capture
	p := c.server(t, srv.URL)
	_, mc := member(t, fp)
	for _, step1 := range []bool{false, true} {
		c.rewrite = nil
		if step1 {
			c.rewrite = stepOneHome
		}
		req, _ := http.NewRequest("POST", p.URL+"/v0/x", strings.NewReader("hello"))
		req.Header.Set("Authorization", "Bearer potluck_tok")
		req.Header.Set("Potluck-As", "mini/abcd1234")
		resp, err := mc.Do(req)
		if err != nil {
			t.Fatalf("step 1 home %v: %v", step1, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 201 || get(resp.Header, "Zm-Team") != "header-value" {
			t.Fatalf("step 1 home %v: %d %s %v", step1, resp.StatusCode, b, resp.Header)
		}
		sent := c.last.Load().Header
		for k := range sent {
			if strings.HasPrefix(strings.ToLower(k), "zm-") {
				t.Errorf("the member sent %s", k)
			}
		}
		for _, k := range []string{"Potluck-Key", "Potluck-Date", "Potluck-Nonce", "Potluck-Sig"} {
			if sent.Get(k) == "" {
				t.Errorf("the member did not send %s", k)
			}
		}
		if sent.Get("Potluck-As") != "mini/abcd1234" {
			t.Errorf("the caller's Potluck-As went as %q", sent.Get("Potluck-As"))
		}
	}

	// An answer that drops its signature is refused, as ever.
	c.rewrite = func(r *http.Response) error { r.Header.Del("Potluck-Home-Sig"); return nil }
	req, _ := http.NewRequest("GET", p.URL+"/v0/x", nil)
	req.Header.Set("Authorization", "Bearer potluck_tok")
	if resp, err := mc.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("an unsigned answer was taken")
	}
}

// The edge secret is taken under both names and Hosts; neither header is
// signed (an edge adds them after the member signed).
func TestEdgeSecretNames(t *testing.T) {
	s := newService(t)
	d := &door{s: s}
	secret := "0123456789abcdef0123"
	if err := s.save(configTunnel(secret)); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "http://x/v0/health", nil)
	req.Header.Set(EdgeSecretHeaderNew, secret)
	if !d.fromEdge(req) {
		t.Fatal("X-Potluck-Edge-Secret not taken")
	}
	req.Header.Del(EdgeSecretHeaderNew)
	req.Host = EdgeHostNew(secret)
	if !d.fromEdge(req) {
		t.Fatal("potluck-edge-<secret> Host not taken")
	}
	if signedHeader(EdgeSecretHeaderNew, true) || signedHeader(EdgeSecretHeader, true) {
		t.Fatal("an edge secret header is signed")
	}
}

func configTunnel(secret string) config.Tunnel { return config.Tunnel{EdgeSecret: secret} }
