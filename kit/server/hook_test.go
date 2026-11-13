package server_test

import (
	"context"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
)

func TestNewRefusesInterruptedRestore(t *testing.T) {
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.MarkRestoring(home, "20261102T010000Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), cfg, nil, wiring.Kit); err == nil || !strings.Contains(err.Error(), "interrupted restore") {
		t.Fatalf("New on a half-restored home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "registry.db")); err == nil {
		t.Fatal("startup created a database in the refused home")
	}
}
