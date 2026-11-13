package e2bgateway

import "github.com/sengirunners/potluck/kit/config"

// UseOldHome points kit/config at the home zm-e2b has always used, zm's
// (ZM_HOME, ~/.zm, zm.sock): kit names no home of its own, and the
// command under its old name stays a client of zm's daemon. cmd/zm-e2b
// calls it first; cmd/potluck-e2b uses potluck's home (cli.UseHome).
func UseOldHome() {
	config.Use(config.Names{HomeEnvs: []string{"ZM_HOME"}, HomeDirs: []string{".zm"}, Sockets: []string{"zm.sock"}})
}
