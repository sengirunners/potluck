package tunnel

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// The tunnel's team transport (core.TeamTransport, SPEC 17.11): a member
// reaches the home at its https URL, signs every request with this daemon's
// key, and takes only answers the pinned home key signed. The code's secret
// never crosses: redemption sends a proof bound to this daemon's key.

// ErrHomeKey is an answer signed by another key than the one the invite
// pinned: a hijacked hostname, never the home (forbidden, not unreachable).
var ErrHomeKey = errors.New("the answer is not signed by the home this invite pinned (key mismatch)")

// ErrUnsigned is an answer with no home signature: the tunnel's own error
// page while the home is down, or something that is not a zm home.
var ErrUnsigned error = unsignedError{}

// unsignedError builds its text when read, because the program's name is set after package init.
type unsignedError struct{}

func (unsignedError) Error() string {
	return "the answer is not signed by a " + config.Program + " home"
}

// TeamTransport is the tunnel's driver.
func (s *Service) TeamTransport() core.TeamTransport { return teamTransport{s} }

type teamTransport struct{ s *Service }

var (
	_ core.TeamTransport = teamTransport{}
	_ core.Sealer        = teamTransport{}
)

// SealTo implements core.Sealer (seal.go).
func (t teamTransport) SealTo(peer string, key []byte, purpose string, ttl time.Duration, plaintext []byte) (string, error) {
	return t.s.sealTo(peer, key, purpose, ttl, plaintext)
}

// OpenFrom implements core.Sealer (seal.go).
func (t teamTransport) OpenFrom(from, purpose, sealed string) ([]byte, error) {
	return t.s.openFrom(from, purpose, sealed)
}

// SealIdentity implements core.Sealer (seal.go).
func (t teamTransport) SealIdentity() (string, string, error) { return t.s.SealIdentity() }

// HomeSealed implements core.Sealer (sealpeers.go).
func (t teamTransport) HomeSealed(fp string) (string, error) { return t.s.HomeSealed(fp) }

// MarkHomeSealed implements core.Sealer (sealpeers.go).
func (t teamTransport) MarkHomeSealed(fp, keyID, mode string) error {
	return t.s.MarkHomeSealed(fp, keyID, mode)
}

// ForgetHomeSealed implements core.Sealer (sealpeers.go).
func (t teamTransport) ForgetHomeSealed(fp string) (bool, error) { return t.s.ForgetHomeSealed(fp) }

// SignSeal implements core.Sealer (sealpeers.go).
func (t teamTransport) SignSeal(msg []byte) (string, error) { return t.s.SignSeal(msg) }

func (t teamTransport) Via() string { return core.ViaTunnel }

func (t teamTransport) ParseCode(code string) (core.JoinTarget, bool, error) {
	c := strings.TrimSpace(code)
	if !strings.HasPrefix(c, "https://") && !strings.HasPrefix(c, "http://") {
		return core.JoinTarget{}, false, nil
	}
	jc, err := project.ParseJoinCode(c)
	if err != nil {
		return core.JoinTarget{}, true, err
	}
	return core.JoinTarget{Via: core.ViaTunnel, URL: jc.URL, Fingerprint: jc.Fingerprint, Secret: jc.Secret, Code: c}, true, nil
}

func (t teamTransport) Prepare(_ context.Context, _ api.JoinRequest, _ string, target core.JoinTarget) (string, string, error) {
	if own, err := t.s.Fingerprint(); err != nil {
		return "", "", err
	} else if own == target.Fingerprint {
		return "", "", api.Errorf(api.CodeBadRequest, "this join code is for this machine's own key").WithHint("run " + config.Command("join") + " on the member's machine")
	}
	return "", "", nil
}

// Extra is a Cloudflare Access service token, when the join names one.
func (t teamTransport) Extra(req api.JoinRequest) (http.Header, error) {
	id, secret := strings.TrimSpace(req.AccessID), strings.TrimSpace(req.AccessSecret)
	if (id == "") != (secret == "") {
		return nil, api.Errorf(api.CodeBadRequest, "a Cloudflare Access service token needs both its id and its secret")
	}
	if id == "" {
		return nil, nil
	}
	return http.Header{"Cf-Access-Client-Id": {id}, "Cf-Access-Client-Secret": {secret}}, nil
}

func (t teamTransport) RedeemRequest(target core.JoinTarget, machine string) (api.RedeemRequest, error) {
	kid, err := t.s.KeyID()
	if err != nil {
		return api.RedeemRequest{}, err
	}
	return api.RedeemRequest{Machine: machine, Proof: project.RedeemProof(target.Secret, target.Fingerprint, kid, machine)}, nil
}

func (t teamTransport) Client(target core.JoinTarget, opts core.ClientOptions) *http.Client {
	return &http.Client{Transport: t.s.Transport(target.Fingerprint, opts.DialTimeout, opts.Extra)}
}

func (t teamTransport) Explain(url string, err error) error {
	var ae *api.Error
	switch {
	case errors.As(err, &ae):
		return ae // the transport's own refusal (sealing: an older home)
	case errors.Is(err, ErrHomeKey):
		return api.Errorf(api.CodeForbidden, "%s", ErrHomeKey.Error()).
			WithHint("something else answers at " + url + " (a hijacked or reused hostname?); ask the home's owner, and for a new invite if it moved")
	case errors.Is(err, ErrUnsigned):
		return api.Errorf(api.CodeUnavailable, "home %s unreachable: %v", url, err).
			WithHint("check the home's daemon and its cloudflared are running")
	}
	return api.Errorf(api.CodeUnavailable, "authority %s unreachable: %v", url, err).
		WithHint("check this machine is online and the home's cloudflared is running")
}

// Invite names this daemon's tunnel URL only while it answers from outside
// as this home (Reachable): a URL in config alone is not enough (a quick
// tunnel's name changes when cloudflared restarts, and a code naming the
// old one admits nobody).
func (t teamTransport) Invite(ctx context.Context) (core.InviteTarget, error) {
	u, fp, err := t.s.Reachable(ctx)
	if err != nil {
		return core.InviteTarget{}, err
	}
	return core.InviteTarget{URL: u, Fingerprint: fp}, nil
}

// RateKey: the key is the node, and anyone can make a key, so wrong proofs
// count per client address (door.clientAddr).
func (t teamTransport) RateKey(rm *core.Remote) string { return "addr:" + rm.Addr }

// RedeemInput: only a proof crosses the tunnel; a code sent in the clear is
// answered like a wrong one.
func (t teamTransport) RedeemInput(ctx context.Context, rm *core.Remote, req api.RedeemRequest) project.RedeemInput {
	in := project.RedeemInput{Node: rm.NodeID, Tagged: rm.Tagged, Machine: req.Machine}
	if fp, err := t.s.Fingerprint(); err == nil && req.Code == "" && t.s.d.Projects != nil {
		in.Hash = t.s.d.Projects.MatchProof(ctx, req.Proof, fp, rm.NodeID, req.Machine)
	}
	return in
}
