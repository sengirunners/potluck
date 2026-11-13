//go:build !darwin && !linux

package cli

import "errors"

// processExe is not read on this system: the caller falls back.
func processExe(int) (string, error) { return "", errors.New("not supported here") }
