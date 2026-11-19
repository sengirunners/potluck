package computeclient_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/protocol"
)

func TestSetRunTargetSecretsResponse(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(api.HeaderProtocol, api.Protocol)
			if r.Method != http.MethodPost || r.URL.Path != "/v0/run_targets/box/handles/h1/secrets" {
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
			if refuse {
				api.WriteError(w, api.Errorf(api.CodeForbidden, "box grant required"))
				return
			}
			api.WriteJSON(w, http.StatusOK, protocol.BoxSecretsResult{Dir: "/dev/shm/potluck-secrets", Count: 2})
		}))
		result, err := computeclient.SetRunTargetSecrets(context.Background(), c, "box", "h1", protocol.BoxSecretsArgs{Secrets: map[string]string{"LOGIN": "sentinel"}})
		if refuse {
			if !api.IsCode(err, api.CodeForbidden) || result != (protocol.BoxSecretsResult{}) {
				t.Fatalf("refusal: %+v %v", result, err)
			}
		} else if err != nil || result.Dir != "/dev/shm/potluck-secrets" || result.Count != 2 {
			t.Fatalf("door response lost: %+v %v", result, err)
		}
	}
}
