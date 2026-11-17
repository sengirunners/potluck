package depscli_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/depscli"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// Under is a project's directory in its checkout when its layer is mounted
// at the default path there; anything else runs at the checkout's top.
func TestUnder(t *testing.T) {
	d, _ := gitProject(t, map[string]string{"packages/a/package-lock.json": lockV3, "packages/a/package.json": "{}"})
	ctx := context.Background()
	sub := filepath.Join(d, "packages", "a")
	def := depscli.DefaultPath(ctx, sub, "checkout")
	for _, tc := range []struct {
		name, dir, dest string
		deps            []computeapi.BoxDeps
		want            string
	}{
		{"at the default path", sub, "checkout", []computeapi.BoxDeps{{Kind: "npm", Path: def}}, "packages/a"},
		{"no deps", sub, "checkout", nil, ""},
		{"a path of its own", sub, "checkout", []computeapi.BoxDeps{{Kind: "npm", Path: "elsewhere"}}, ""},
		{"an absolute checkout", sub, "/work", []computeapi.BoxDeps{{Kind: "npm", Path: "/work/packages/a"}}, ""},
		{"the top", d, "checkout", []computeapi.BoxDeps{{Kind: "npm", Path: "checkout"}}, ""},
	} {
		if got := depscli.Under(ctx, tc.dir, tc.dest, tc.deps); got != tc.want {
			t.Errorf("%s: Under = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ImageOK refuses --image with --deps on a binding another machine serves
// (it takes a template), and nothing else.
func TestImageOK(t *testing.T) {
	views := map[string]computeapi.RunTargetView{
		"lent":  {RunTarget: computeapi.RunTarget{Name: "lent", Kind: "box", Host: "mini", HostBinding: "mini-c2m4"}},
		"local": {RunTarget: computeapi.RunTarget{Name: "local", Kind: "box"}},
	}
	c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		api.WriteJSON(w, 200, views[strings.TrimPrefix(r.URL.Path, "/v0/run_targets/")])
	}))
	ctx := context.Background()
	deps := []computeapi.BoxDeps{{Kind: "npm", Path: "checkout"}}
	if err := depscli.ImageOK(ctx, c, "lent", "img", deps); err == nil || !strings.Contains(err.Error(), "takes a template (--template") {
		t.Errorf("lent with an image: %v", err)
	}
	for _, tc := range []struct {
		target, image string
		deps          []computeapi.BoxDeps
	}{{"local", "img", deps}, {"lent", "", deps}, {"lent", "img", nil}} {
		if err := depscli.ImageOK(ctx, c, tc.target, tc.image, tc.deps); err != nil {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}
