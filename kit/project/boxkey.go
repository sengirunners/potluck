package project

import (
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/sealbox"
	"github.com/sengirunners/potluck/kit/store"
)

// A box's token (the potluck-token kind) is bound to the key id of a key
// made for that box alone, in tokens.box_key. It is a column of its own,
// not tokens.node: a node-bound token is a team replica's, which stands for
// the whole member (it may link a GitHub login, leave a team), and a box's
// token must never be read as one.
func init() {
	store.RegisterRegistry("project/0007_token_box_key", `
ALTER TABLE tokens ADD COLUMN box_key TEXT NOT NULL DEFAULT '';`)
}

// checkBoxKey refuses a box key that is not a key id, or a box token with
// no project to cap it to.
func checkBoxKey(req api.MintTokenRequest) error {
	if req.BoxKey == "" {
		return nil
	}
	// A key id (sealbox.KeyID): a tailnet node ID never has its shape, so
	// a box's token is refused on the tailnet.
	if !sealbox.IsKeyID(req.BoxKey) {
		return api.Errorf(api.CodeBadRequest, "box key %q is not a key id", req.BoxKey)
	}
	if req.Project == "" {
		return api.Errorf(api.CodeBadRequest, "a box's token is capped to a project")
	}
	return nil
}
