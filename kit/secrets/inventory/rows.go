package inventory

// Bead ids named by the table. Each is the owner of a verdict in the
// decision, not a dependency of this package.
const (
	beadAppKeyHelper  = "zm-eakh.12.54.13" // the runner App key moves behind the locked-down account
	beadEscrow        = "zm-eakh.12.48.40.15"
	beadRotation      = "zm-eakh.12.48.40.16"
	beadCloudflared   = "zm-eakh.12.48.40.29"
	beadInventoryRows = "zm-eakh.12.48.40.4"
)

// FileAt, ConfigRefAt and ConfigValueAt build Locations for a program's own rows.
func FileAt(p string) Location        { return Location{KindFile, p} }
func ConfigRefAt(p string) Location   { return Location{KindConfigRef, p} }
func ConfigValueAt(p string) Location { return Location{KindConfigValue, p} }

// Potluck is the table of the keys Potluck itself keeps in a home, with the
// known exception: the runner App key is class S and sits in a 0600 file
// until zm-eakh.12.54.13 moves it. A program with keys of its own adds them
// with Table.With.
func Potluck() Table {
	return Table{
		Rows: []Row{
			{ID: "home-key", Name: "home key (ed25519)", Class: ClassO, Bead: beadEscrow,
				Locations: []Location{FileAt("tunnel/key"), ConfigRefAt("tunnel.key")}},
			{ID: "seal-key", Name: "seal key (X25519)", Class: ClassO, Bead: beadInventoryRows,
				Locations: []Location{FileAt("tunnel/seal")}},
			{ID: "runner-app-key", Name: "runner GitHub App private key", Class: ClassS, Bead: beadAppKeyHelper,
				WarnWithAgents: true,
				Locations:      []Location{FileAt("secrets/runners/app-*-key.pem"), ConfigRefAt("runners.github.private_key")}},
			{ID: "runner-app-client-secret", Name: "runner GitHub App client secret", Class: ClassO, Bead: beadRotation,
				Locations: []Location{FileAt("secrets/runners/app-*-client_secret"), ConfigRefAt("runners.github.client_secret")}},
			{ID: "runner-app-webhook-secret", Name: "runner GitHub App webhook secret", Class: ClassO, Bead: beadRotation,
				Locations: []Location{FileAt("secrets/runners/app-*-webhook_secret"), ConfigRefAt("runners.github.webhook_secret")}},
			// An issuer key that is the runner App's key is that file, and the
			// runner-app-key row judges it (class S). Any other is O.
			{ID: "issuer-key", Name: "secrets issuer GitHub App private key", Class: ClassO, Bead: beadAppKeyHelper,
				Locations: []Location{ConfigRefAt("secrets.issuers.*.private_key")}},
			{ID: "member-token", Name: "member token of a team this home joined", Class: ClassO, Bead: beadInventoryRows,
				Locations: []Location{FileAt("remotes/*.token"), FileAt("remotes/*.headers")}},
			{ID: "github-lookup-token", Name: "GitHub lookup token (read:org)", Class: ClassO, Bead: beadInventoryRows,
				Locations: []Location{ConfigRefAt("github.lookup_token")}},
			{ID: "tunnel-edge-secret", Name: "tunnel edge secret", Class: ClassO, Bead: beadInventoryRows,
				Locations: []Location{
					ConfigValueAt("tunnel.edge_secret"),
					FileAt("tunnel/cloudflared/state.json"), FileAt("tunnel/cloudflared/config.yml"),
				}},
			{ID: "cloudflared-login", Name: "cloudflared login certificate", Class: ClassO, Bead: beadCloudflared,
				Locations: []Location{FileAt("tunnel/cloudflared/home/.cloudflared/cert.pem")}},
			{ID: "cloudflared-tunnel-credentials", Name: "cloudflared tunnel credentials", Class: ClassO, Bead: beadInventoryRows,
				Locations: []Location{FileAt("tunnel/cloudflared/credentials.json")}},
			{ID: "tailnet-node-key", Name: "tailnet node key and certificate keys", Class: ClassO, Bead: beadInventoryRows,
				Locations: []Location{FileAt("tsnet/tailscaled.state"), FileAt("tsnet/certs/*.key"), FileAt("tsnet/certs/*.key.pem")}},
			{ID: "e2b-gateway-secrets", Name: "e2b gateway CA key and token secret", Class: ClassO, Bead: beadInventoryRows,
				Locations: []Location{FileAt("e2b/ca-key.pem"), FileAt("e2b/token-secret")}},
			{ID: "e2b-keys", Name: "e2b keys file", Class: ClassO, Bead: beadInventoryRows,
				Locations: []Location{FileAt("e2b/keys")}},
		},
		// Every file under these is a key. Other directories hold keys next
		// to files that are not (tunnel/, tsnet/ hold logs and caches), so
		// their keys are found only through a row.
		Roots: []string{"secrets", "remotes", "e2b"},
		// Public files next to keys: the e2b gateway's CA certificates, and
		// a file manager's folder note.
		Ignore: []string{"e2b/ca.pem", "e2b/ca-bundle.pem", "**/.DS_Store"},
		Exceptions: map[string]string{
			"runner-app-key": beadAppKeyHelper,
		},
	}
}
