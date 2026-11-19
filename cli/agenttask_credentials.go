package cli

import (
	"context"
	"fmt"

	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/protocol"
)

func spawnAgentTask(ctx context.Context, box daemonBox, argv []string, name, value string, timeoutS int, key string) (string, error) {
	_, err := computeclient.SetRunTargetSecrets(ctx, box.c, box.target, box.id, protocol.BoxSecretsArgs{Secrets: map[string]string{name: value}})
	if err != nil {
		return "", fmt.Errorf("deliver the agent credential through the box secrets door: %w", err)
	}
	return box.Spawn(ctx, argv, nil, timeoutS, key)
}
