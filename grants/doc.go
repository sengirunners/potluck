// Package grants is the policy part of the secrets door: a table on a
// pool's authority (project.db, secret_grants) that says who may put which
// kind of secret, up to what ceiling, into which boxes, and the check every
// secret-bearing call makes against it before anything is sent to a driver
// or a host. docs/daemon.md, "Secret grants", is the user's side.
//
// A grant: Area (whose boxes; "" every area), To (an identity by id, a
// role on the box's area, or a GitHub job the broker serves), Kind (value,
// github-jit, or an issuer's kind), Name (the file name in the box; "*" for
// any name of a value grant), Ceiling (value: {"max_bytes"} per value;
// github-jit: {}; an issuer's kind: its issuer's), HostClass (trusted
// only), Scopes (exec, box) and Targets (optional run targets). No grant,
// no secret, except the built-in Defaults (value on exec and spawn,
// github-jit on acquire), which keep today's behaviour until the pool's
// owner turns them off. A public host gets nothing, whatever a row says.
//
// Seams for the beads that follow:
//
//   - Check(ctx, d, Request) is the one check. Run targets call it for
//     exec and spawn (kind value, scope exec) and for a jit_config on
//     acquire (kind github-jit, scope box). The box-scope secrets verb calls
//     it with Scope computeapi.SecretScopeBox; an issuer's caller with Kind,
//     Name and Params of the minted secret, and hands Decision.Grant and
//     Decision.Params to the issuer; the broker's job grants with
//     Request.Job set from GitHub's answers about the run. For caller-held
//     kinds, Granted just before the call that carries the secrets (if the
//     ledger cannot take it nothing is sent) and Delivered once it answered;
//     for an issuer's kind both do nothing (the issuer writes its own).
//   - Issuer is what an issuer's kind implements for the check: Kind,
//     Ceiling and Within. ProvideIssuer registers one on the daemon's Deps;
//     until then no grant of its kind can be added and a request for it is
//     refused. OnGrantRemoved registers a hook told when a grant is removed
//     (or the defaults turned off), so an issuer ends what it issued.
//     issuers.Register does both for each minted kind's issuer, and
//     issuers.GrantCheck is its Options.Check over Check.
//
// The ledger is the project's event log (kit/events): secret.granted,
// secret.delivered and secret.refused per request (audience: operate on
// the box's area), secret_grant.* per edit (admin). An entry carries
// names, sizes and policy, never a value; the package never sees a value.
//
// A daemon that wires run targets (runtarget.New) gets the check whether or
// not it wires this module: Check reads the table itself and enforces the
// defaults. To let people see and change grants it wires New(d) beside
// runtarget.New, after teamlink is provided (wiring.Compute does), and its
// route list documents the four /v0/secret_grants routes.
package grants
