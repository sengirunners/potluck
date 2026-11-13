// Package swap keeps a daemon swap (stop, restart, upgrade) from breaking
// what this machine serves (SPEC 2, "Swaps").
// Owned by the integrator.
//
//   - lock.go: swap.lock, one swap per ZM_HOME, and who holds it;
//   - manifest.go: what a build can serve, what a home has it serve, and
//     the preflight that compares them before a binary is swapped in;
//   - probe.go: the served set (the reads bd and zm make), compared before
//     and after a swap and at every start;
//   - files.go: placing new binaries with each previous one kept as
//     <name>.prev, and putting them back.
//
// It imports no module: the daemon (kit/server) hands it routes and
// proxy rules, and it reads other daemons only through kit/client.
package swap
