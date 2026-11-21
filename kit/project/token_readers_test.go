package project

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// tokenReaders is every function in this module whose SQL names the tokens
// table, with what a limited row (route_limit set) is to it. A limited token
// must reach nothing that did not decide: a new reader fails
// TestTokenReaders until it is added here with its decision, and the
// decision is tested where it says.
var tokenReaders = map[string]string{
	// The one authenticator: device, grant and shape checks (TestLimitedTokenUse).
	"kit/project/registry.go FromTokenCaller": "admits only at its pinned device, with its grant",
	// Mints and the owner's own lists and revocations, by id.
	"kit/project/registry.go Mint":            "writes plain rows only (refuses a limit)",
	"kit/project/limited.go MintLimited":      "writes the limited row",
	"kit/project/registry.go Tokens":          "lists it with its limit and device (local only)",
	"kit/project/registry.go RevokeToken":     "revokes it by id (local only)",
	"kit/project/registry.go RevokeTokenHash": "plain hash only: never finds it",
	// Subject and liveness readers with no route or device to check: refuse.
	"kit/project/registry.go TokenExpiry":             "refuses it (route_limit = '')",
	"kit/project/team.go TokenLive":                   "refuses it (route_limit = '')",
	"kit/project/team.go TokenScope":                  "refuses it (route_limit = '')",
	"kit/project/subscription.go SubscriptionSubject": "refuses it (limit != '')",
	// Node-bound (replica) and box readers: a limited row has no node,
	// machine or box key (MintLimited writes none), so none selects it.
	"kit/project/team.go JoinedNodes":     "node-bound only",
	"kit/project/team.go Redeem":          "by its own invite's token id: never a limited one",
	"kit/project/team.go nodeLive":        "node-bound only",
	"kit/project/team.go endedInvites":    "node-bound only (by a redeemed invite's node)",
	"kit/project/team.go RevokeOwn":       "node-bound only",
	"kit/project/team.go MachineProjects": "node-bound only",
	"kit/project/team.go MachineJoin":     "node-bound only",
	"kit/project/team.go InvitedMachines": "node-bound only",
	"kit/project/team.go HeldTokensIn":    "refuses it (route_limit = ''; a limited secret is skipped): a parked standby's owner, kit/teamlink home_owner.go",
	"kit/project/admit.go Admit":          "node-bound only (Admit refuses an empty node)",
	"kit/project/admit.go admitTx":        "writes a node-bound row",
	"kit/project/admit.go freshJoin":      "node-bound only",
	// Pool removal: ends every live token of the pool, a limited one too.
	"kit/project/remove.go Remove": "ends it with the pool's other tokens (by project)",
	// Schema.
	"kit/project/registry.go init": "schema",
	"kit/project/team.go init":     "schema",
	"kit/project/boxkey.go init":   "schema",
	"kit/project/limited.go init":  "schema",
	"kit/project/remove.go init":   "schema",
}

var tokensSQL = regexp.MustCompile(`(?i)\b(from|join|update|into|table)\s+tokens\b`)

// TestTokenReaders lists the SQL readers of the tokens table across the
// module (only the source says where SQL is), so that each decides what a
// limited row is.
func TestTokenReaders(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal("module root not found: ", err)
	}
	found := map[string]bool{}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (strings.HasPrefix(n, ".") || n == "testdata" || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			name := "package level"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				name = fd.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && tokensSQL.MatchString(lit.Value) {
					found[rel+" "+name] = true
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range found {
			if !yield(k) {
				return
			}
		}
	}) {
		if _, ok := tokenReaders[k]; !ok {
			t.Errorf("%s reads the tokens table: add it to tokenReaders with what a limited row is to it", k)
		}
	}
	for k := range tokenReaders {
		if !found[k] {
			t.Errorf("%s no longer reads the tokens table: remove it from tokenReaders", k)
		}
	}
}
