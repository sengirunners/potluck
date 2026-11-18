package project

import (
	"encoding/base64"
	"net/url"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// An invite link carries a join code in its fragment: <base>#v1.<the code,
// base64url without padding>. The fragment never reaches the web server
// that serves the page, and base64url survives chat apps' link detection (a
// tunnel code is itself https://<host>#<fp>:<secret>). The program that
// mounts the commands chooses the base (its own join page); a link is known
// by its shape, never by its host.
const (
	joinLinkTag = "v1."
	// maxJoinLink bounds what is decoded: a code is under 200 characters.
	maxJoinLink = 4096
)

// JoinLink is the invite link for code under base (a page's https URL, such
// as https://example.com/join).
func JoinLink(base, code string) string {
	return base + "#" + joinLinkTag + base64.RawURLEncoding.EncodeToString([]byte(code))
}

// JoinCodeOf is the join code in s: the code inside an invite link, or s
// itself when s is not one. A link that is malformed, of an unknown version,
// or wraps another link is refused.
func JoinCodeOf(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !looksLikeJoinLink(s) {
		return s, nil
	}
	bad := api.Errorf(api.CodeBadRequest, "this invite link is incomplete or damaged: copy the whole link again, or ask for a new one")
	if len(s) > maxJoinLink {
		return "", bad
	}
	_, frag, _ := strings.Cut(s, "#")
	enc, ok := strings.CutPrefix(frag, joinLinkTag)
	if !ok {
		return "", api.Errorf(api.CodeBadRequest, "this invite link is from a newer version: update potluck (potluck upgrade), then join again")
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", bad
	}
	code := strings.TrimSpace(string(raw))
	if code == "" || looksLikeJoinLink(code) || strings.ContainsFunc(code, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		return "", bad
	}
	return code, nil
}

// looksLikeJoinLink: an https URL whose path ends in /join and that has a
// fragment. A tunnel code never has a path (TunnelURL refuses one).
func looksLikeJoinLink(s string) bool {
	if !strings.HasPrefix(s, "https://") || !strings.Contains(s, "#") {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Host != "" && strings.HasSuffix(u.Path, "/join")
}
