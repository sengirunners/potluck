package core

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandHome turns a leading ~ or ~/ into the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// PathExists reports whether p can be stat'ed.
func PathExists(p string) bool { _, err := os.Stat(p); return err == nil }

// ClipText cuts s to n bytes, marking the cut.
func ClipText(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
