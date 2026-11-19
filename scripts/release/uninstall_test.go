package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/cli"
)

// potluck uninstall removes the programs a release installs, and no others.
func TestUninstallNamesTheProgramsAReleaseInstalls(t *testing.T) {
	shipped := map[string]bool{}
	for _, target := range Targets {
		goos, goarch, _ := strings.Cut(target, "/")
		for _, n := range commandsFor(goos, goarch) {
			shipped[n] = true
			if !slices.Contains(cli.InstalledPrograms, n) {
				t.Errorf("%s ships %s, which potluck uninstall does not remove", target, n)
			}
		}
	}
	for _, n := range cli.InstalledPrograms {
		if !shipped[n] {
			t.Errorf("potluck uninstall removes %s, which no release ships", n)
		}
	}
}
