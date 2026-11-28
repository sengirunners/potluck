package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/protocol"
)

func TestAgentTaskDoorBeforeSpawn(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		var calls []string
		c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(api.HeaderProtocol, api.Protocol)
			calls = append(calls, r.URL.Path)
			switch {
			case strings.HasSuffix(r.URL.Path, "/secrets"):
				var req protocol.BoxSecretsArgs
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Secrets["CLAUDE_CODE_OAUTH_TOKEN"] != "sentinel" {
					t.Errorf("door payload: %+v %v", req, err)
				}
				if refuse {
					http.Error(w, `{"error":{"code":"forbidden","message":"no box grant"}}`, http.StatusForbidden)
					return
				}
				json.NewEncoder(w).Encode(protocol.BoxSecretsResult{Dir: "/dev/shm/potluck-secrets", Count: 1})
			case strings.HasSuffix(r.URL.Path, "/spawn"):
				var req computeapi.SpawnRunTargetRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Secrets) != 0 || !slices.Equal(req.Argv, []string{"agent"}) {
					t.Errorf("spawn payload: %+v %v", req, err)
				}
				json.NewEncoder(w).Encode(computeapi.RunTargetSpawn{PID: "p1"})
			default:
				t.Errorf("unexpected route %s", r.URL.Path)
			}
		}))
		pid, err := spawnAgentTask(context.Background(), daemonBox{c: c, target: "box", id: "h1"}, nil, []string{"agent"}, "CLAUDE_CODE_OAUTH_TOKEN", "sentinel", 60, "request")
		if refuse {
			if err == nil || pid != "" || len(calls) != 1 {
				t.Fatalf("refusal: pid=%s err=%v calls=%v", pid, err, calls)
			}
		} else if err != nil || pid != "p1" || len(calls) != 2 {
			t.Fatalf("spawn: pid=%s err=%v calls=%v", pid, err, calls)
		}
	}
}
