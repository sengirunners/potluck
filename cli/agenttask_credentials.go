package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/protocol"
)

func spawnAgentTask(ctx context.Context, box daemonBox, timing *jobTiming, argv []string, name, value string, timeoutS int, key string) (string, error) {
	step := time.Now()
	_, err := computeclient.SetRunTargetSecrets(ctx, box.c, box.target, box.id, protocol.BoxSecretsArgs{Secrets: map[string]string{name: value}})
	timing.startStep("secrets", step)
	if err != nil {
		return "", fmt.Errorf("deliver the agent credential through the box secrets door: %w", err)
	}
	step = time.Now()
	defer timing.startStep("spawn", step)
	return box.Spawn(ctx, argv, nil, timeoutS, key)
}
