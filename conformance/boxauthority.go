package conformance

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/protocol"
)

// Group box-authority (docs/daemon.md "Calling the authority from a box"):
// a program in a box reaches its authority with only the credential the
// potluck-token kind delivers (one file in $POTLUCK_SECRETS: a token, the
// per-box key it is bound to, the authority's tunnel URL and key). It runs
// from inside the box, through the driver's secrets verb and exec, so a
// driver that does not land the file, or keeps it after unset, fails here.

// BoxAuthority is an authority a box can reach and the client a box calls
// it with.
type BoxAuthority struct {
	// Credential is a box credential file's content (tunnel.BoxCredential)
	// naming an authority this driver's boxes can reach: what the
	// potluck-token kind would deliver.
	Credential string
	// Client is the command run in the box with $POTLUCK_BOX_CREDENTIAL
	// naming the landed file (potluck status --json, say). It must exit 0
	// and print Want when it reached the authority.
	Client []string
	Want   string
}

// validate refuses an authority the group could not use.
func (a *BoxAuthority) validate(kind string) error {
	switch {
	case kind != "" && kind != "box":
		return fmt.Errorf("--box-credential is a box target's (this one is a %s)", kind)
	case len(a.Client) == 0 || a.Want == "":
		return fmt.Errorf("--box-credential needs --box-client and --box-want")
	}
	if _, err := tunnel.ParseBoxCredential(a.Credential); err != nil {
		return fmt.Errorf("--box-credential: %v", err)
	}
	return nil
}

const (
	baReach = "box authority: a command in the box reaches its authority with only the credential delivered into it"
	baGone  = "box authority: once the credential is unset, the same command in the box reaches nothing"
)

var baChecks = []string{baReach, baGone}

// baName is the credential's name in the box.
const baName = "ZMCONF_BOX_CREDENTIAL"

// baLine runs the client with $POTLUCK_BOX_CREDENTIAL naming the file.
const baLine = `POTLUCK_BOX_CREDENTIAL="${POTLUCK_SECRETS:-$ZM_SECRETS}/` + baName + `" exec "$@"`

func (r *run) boxAuthorityGroup() {
	if r.kind != "box" {
		return
	}
	a := r.opt.BoxAuthority
	if a == nil {
		for _, c := range baChecks {
			r.skip(c, "no authority given (Options.BoxAuthority)")
		}
		return
	}
	if !slices.Contains(boxCaps(r), protocol.CapBoxSecrets) {
		for _, c := range baChecks {
			r.skip(c, "describe lists no box-secrets capability")
		}
		return
	}
	// Never in a result: the file, and the token inside it.
	r.secrets = append(r.secrets, a.Credential)
	if c, err := tunnel.ParseBoxCredential(a.Credential); err == nil {
		r.secrets = append(r.secrets, c.Token, hex.EncodeToString(c.Key.Seed()))
	}
	r.sawSecret = true
	h, err := r.acquireAny(r.longTTL())
	if err != nil {
		for _, c := range baChecks {
			r.add(c, fmt.Errorf("acquire a box: %v", err))
		}
		return
	}
	defer r.endHeld(h)
	if _, err := r.setBoxSecrets(h.Handle, map[string]any{"secrets": map[string]string{baName: a.Credential}}); err != nil {
		for _, c := range baChecks {
			r.add(c, fmt.Errorf("secrets: %v", err))
		}
		return
	}
	argv := append([]string{"sh", "-c", baLine, "sh"}, a.Client...)
	res, err := r.exec(h.Handle, argv, "", 0)
	if err == nil && (*res.Exit != 0 || !strings.Contains(*res.Stdout, a.Want)) {
		err = fmt.Errorf("the client exited %d, printing %q (want %q; stderr %q)", *res.Exit, clip(*res.Stdout), a.Want, clip(*res.Stderr))
	}
	if !r.add(baReach, err) {
		r.skip(baGone, "the box never reached its authority")
		return
	}
	if _, err := r.setBoxSecrets(h.Handle, map[string]any{"unset": []string{baName}}); err != nil {
		r.add(baGone, fmt.Errorf("unset: %v", err))
		return
	}
	res, err = r.exec(h.Handle, argv, "", 0)
	if err == nil && (*res.Exit == 0 || strings.Contains(*res.Stdout, a.Want)) {
		err = fmt.Errorf("the client still reached the authority (exit %d, printing %q)", *res.Exit, clip(*res.Stdout))
	}
	if err == nil {
		err = r.execExit(h.Handle, 0, "sh", "-c", `test ! -e "${POTLUCK_SECRETS:-$ZM_SECRETS}/`+baName+`"`)
	}
	r.add(baGone, err)
}
