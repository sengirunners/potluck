package runtarget

// Swap and box secrets. A box's memory filesystem holds its secrets; if the
// host swaps to a plain disk, they can be written there. A host reads its
// own swap (hostswap) into its adverts; the authority gives box secrets only
// to a host whose swap is safe. A host on a build from before the field
// reports none, which is not safe: it keeps its secret-free work, and gets
// secrets once it is updated (a doctor run there says so).

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/api"
)

// swapFresh is how long a reading of this machine's swap serves the next
// beats before the machine is asked again.
const swapFresh = 30 * time.Second

// swapCache is the last reading of this machine's swap.
type swapCache struct {
	at    time.Time
	state string
}

// localSwap is the state of this machine's swap, for its adverts.
func (s *Service) localSwap(ctx context.Context) string {
	s.host.swapMu.Lock()
	defer s.host.swapMu.Unlock()
	if s.host.swap.state == "" || time.Since(s.host.swap.at) > swapFresh {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		state := hostswap.Local(rctx).State
		if rctx.Err() != nil {
			return hostswap.Unknown // cut short: a reading no later beat should inherit
		}
		s.host.swap = swapCache{at: time.Now(), state: state}
	}
	return s.host.swap.state
}

// swapRefusal is why the host behind advert is given no box secrets, nil
// when its swap is safe. It names the host and the fix, never what was sent.
func swapRefusal(machine string, d computeapi.RunTargetDescribe) *api.Error {
	switch {
	case hostswap.Safe(d.Swap):
		return nil
	case d.Swap == "":
		return api.Errorf(api.CodeForbidden, "%s does not report its swap state (an older build of Potluck): it is given no box secrets, only secret-free work, until it is updated; "+
			"update Potluck there and run potluck doctor", machine)
	case d.Swap == hostswap.PlainDisk:
		return api.Errorf(api.CodeForbidden, "%s swaps to a plain disk, where a box's secrets could be written: it is given no box secrets, only secret-free work; fix swap there: %s", machine, hostswap.Fix)
	}
	return api.Errorf(api.CodeForbidden, "%s cannot say its swap is safe for a box's secrets: it is given no box secrets, only secret-free work; check swap there with potluck doctor: %s", machine, hostswap.Fix)
}

// setsSecrets says whether op args set a secret: a non-empty secrets field,
// under any spelling encoding/json reads as it. A body that is not a JSON
// object counts as setting one. A jit_config or an unset writes no secret
// into the memory filesystem.
func setsSecrets(args json.RawMessage) bool {
	if len(bytes.TrimSpace(args)) == 0 {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(args, &obj) != nil {
		return true
	}
	for k, v := range obj {
		if t := string(bytes.TrimSpace(v)); strings.EqualFold(k, "secrets") && t != "" && t != "null" && t != "{}" {
			return true
		}
	}
	return false
}

// BoxSwapSafe refuses when project's box handle is on a host whose swap is
// not safe for a box secret (rt.Door): a credential issuer asks before it
// mints, so it never mints what the door would then refuse.
func (s *Service) BoxSwapSafe(ctx context.Context, project, handle string) error {
	b, _, err := s.boxOf(ctx, project, handle)
	if err != nil {
		return err
	}
	return s.hostSwapSafe(ctx, b)
}

// hostSwapSafe refuses a box secret for a binding served from another
// machine whose advert does not show safe swap. A binding on this machine
// is read by potluck doctor (it fails on unsafe swap) and is not
// gated here.
func (s *Service) hostSwapSafe(ctx context.Context, b computeapi.RunTarget) error {
	if b.Host == "" {
		return nil
	}
	d, err := s.remoteDescribe(ctx, b)
	if err != nil {
		return err
	}
	if e := swapRefusal(b.Host, *d); e != nil {
		return e
	}
	return nil
}
