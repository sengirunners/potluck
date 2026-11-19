package e2bgateway

import "github.com/sengirunners/potluck/kit/config"

// UseOldHome points kit/config at the home zm-e2b has always used, zm's
// (ZM_HOME, ~/.zm, zm.sock), and its messages at zm's commands (Potluck's
// under zm's word potluck, as zm mounts them): kit names
// no home of its own, and the command under its old name stays a client of
// zm's daemon. cmd/zm-e2b calls it first; cmd/potluck-e2b uses potluck's
// home and words (cli.UseHome).
func UseOldHome() {
	config.Use(config.Names{HomeEnvs: []string{"ZM_HOME"}, HomeDirs: []string{".zm"}, Sockets: []string{"zm.sock"}})
	config.Program, config.Prefix, config.Lacks = "zm", "zm potluck", nil
}
