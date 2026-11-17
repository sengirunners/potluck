package cli

// potluck secrets grant add|ls|rm|default: the pool's secret grants (the
// policy table on the pool's machine, package grants). A job grant has no
// flags yet: it is for a minted kind, which no issuer serves yet.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
)

func (a *app) secretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Who may put which secrets in the pool's sandboxes: grant",
	}
	grant := &cobra.Command{
		Use:   "grant",
		Short: "The pool's secret grants: add, ls, rm, default",
		Long: `A secret grant says who may put which kind of secret, up to what size, into
which of the pool's sandboxes. No grant, no secret, except the built-in
default: anyone who may use a sandbox may send their own values to it on
` + a.cmd("sandbox exec") + ` (--secret), as before grants existed. The pool's owner
turns the default off (` + a.cmd("secrets grant default off") + `) once the grants
the pool needs are added. A public host is given no secrets, whatever a
grant says.`,
	}
	grant.AddCommand(a.grantAddCmd(), a.grantLsCmd(), a.grantRmCmd(), a.grantDefaultCmd())
	cmd.AddCommand(grant)
	return cmd
}

func (a *app) grantAddCmd() *cobra.Command {
	var req computeapi.AddSecretGrantRequest
	var maxBytes int
	var ceiling string
	var scopes []string
	c := &cobra.Command{
		Use:   "add --name NAME (--identity NAME | --role member|owner) [--area A] [--target T] [--max-bytes N]",
		Short: "Add a grant (an admin of its area)",
		Long: `Adds a grant. --name is the secret's file name in the sandbox
([A-Z][A-Z0-9_]{0,63}), or * for any name. --identity names one person or
agent in the pool; --role member covers everyone who may use the area's
sandboxes. --area limits it to one pool area (default: every area, which
only the pool's owner may grant). --target limits it to shared machine
sizes, as ` + a.cmd("status") + ` lists them (--on's names). --scope is exec (one
command, the default) or sandbox (the sandbox's whole life; nothing sends
those yet).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			for _, sc := range scopes {
				if sc == "sandbox" { // the API's word is box
					sc = computeapi.SecretScopeBox
				}
				req.Scopes = append(req.Scopes, sc)
			}
			switch {
			case ceiling != "" && maxBytes > 0:
				return fmt.Errorf("give --max-bytes or --ceiling, not both")
			case ceiling != "":
				req.Ceiling = json.RawMessage(ceiling)
			case maxBytes > 0:
				req.Ceiling, _ = json.Marshal(map[string]int{"max_bytes": maxBytes})
			}
			g, err := computeclient.AddSecretGrant(cmd.Context(), cl, req)
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(g)
			}
			a.say("%s", grantLine(g))
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&req.Name, "name", "", "the secret's file name in the sandbox, or * for any (kind value)")
	f.StringVar(&req.Kind, "kind", computeapi.SecretKindValue, "value (a secret the sender holds); minted kinds need their issuer")
	f.StringVar(&req.To.Identity, "identity", "", "who may send it: one identity in the pool")
	f.StringVar(&req.To.Role, "role", "", "who may send it: everyone with this role on the sandbox's area (member or owner)")
	f.StringVar(&req.Area, "area", "", "the pool area whose sandboxes it covers (default: every area)")
	f.StringArrayVar(&req.Targets, "target", nil, "only sandboxes of these shared machine sizes, as --on names them (repeatable)")
	f.StringArrayVar(&scopes, "scope", nil, "exec (default) or sandbox (repeatable)")
	f.IntVar(&maxBytes, "max-bytes", 0, "kind value: the largest value it allows (default 65536)")
	f.StringVar(&ceiling, "ceiling", "", "the ceiling as JSON (a minted kind's)")
	_ = c.MarkFlagRequired("name")
	return c
}

func (a *app) grantLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the default and the grants in the areas you can see",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			l, err := computeclient.SecretGrants(cmd.Context(), cl)
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(l)
			}
			for _, g := range l.Grants {
				a.say("%s", grantLine(g))
			}
			if l.Note != "" {
				a.say("(%s)", l.Note)
			}
			return nil
		},
	}
}

func (a *app) grantRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Remove a grant (an admin of its area)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			g, err := computeclient.RemoveSecretGrant(cmd.Context(), cl, args[0])
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(g)
			}
			a.say("removed %s", grantLine(g))
			return nil
		},
	}
}

func (a *app) grantDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "default on|off",
		Short: "Turn the built-in grant on or off (the pool's owner)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			on := args[0] == "on"
			if !on && args[0] != "off" {
				return fmt.Errorf("default takes on or off, not %q", args[0])
			}
			cl, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			g, err := computeclient.SetSecretGrantDefault(cmd.Context(), cl, on)
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(g)
			}
			a.say("%s", grantLine(g))
			return nil
		},
	}
}

// grantLine is one grant for people.
func grantLine(g computeapi.SecretGrant) string {
	to := ""
	switch {
	case g.To.Identity != "":
		to = "identity " + g.To.Identity
	case g.To.Role != "":
		to = "role " + g.To.Role
	case g.To.Job != nil:
		to = fmt.Sprintf("job %s %s %s (%s)", g.To.Job.Repo, g.To.Job.Workflow, g.To.Job.Ref, strings.Join(g.To.Job.Events, ","))
	}
	area := g.Area
	if area == "" {
		area = "every area"
	}
	scopes := strings.ReplaceAll(strings.Join(g.Scopes, ","), computeapi.SecretScopeBox, "sandbox")
	name := g.Name
	if name == "" {
		name = "(a name withheld over the tunnel)"
	}
	s := fmt.Sprintf("%s: %s may put %s %s in %s (%s hosts, scope %s", g.ID, to, g.Kind, name, area, g.HostClass, scopes)
	if len(g.Targets) > 0 {
		s += ", targets " + strings.Join(g.Targets, ",")
	}
	if len(g.Ceiling) > 0 {
		s += ", ceiling " + string(g.Ceiling)
	}
	s += ")"
	if g.Builtin {
		s += " [built in]"
	}
	if g.Off {
		s += " [off]"
	}
	return s
}
