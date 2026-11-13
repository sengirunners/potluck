package tunnel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The record of homes that sealed: kept with the mode they reported, a
// readable-by-others file refused, a corrupt one moved aside and failing
// closed until forget starts a new record.
func TestSealPeersRecord(t *testing.T) {
	s := newService(t)
	path := filepath.Join(s.d.Config.Home, SealPeersFile)
	if m, err := s.HomeSealed("fp1"); m != "" || err != nil {
		t.Fatalf("empty: %q %v", m, err)
	}
	if err := s.MarkHomeSealed("fp1", "zmkey:a", "preferred"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkHomeSealed("fp2", "zmkey:b", ""); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.HomeSealed("fp1"); m != "preferred" {
		t.Fatalf("fp1: %q", m)
	}
	if m, _ := s.HomeSealed("fp2"); m != "required" {
		t.Fatalf("fp2 (no mode reported): %q", m)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HomeSealed("fp1"); err == nil || !strings.Contains(err.Error(), "chmod 600 "+path) {
		t.Fatalf("readable by others: %v", err)
	}
	_ = os.Chmod(path, 0o600)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.HomeSealed("fp1"); err == nil || !strings.Contains(err.Error(), ".corrupt") || !strings.Contains(err.Error(), "potluck sealing forget") {
			t.Fatalf("corrupt: %v", err)
		}
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("not moved aside: %v", err)
	}
	if err := s.MarkHomeSealed("fp3", "zmkey:c", "preferred"); err == nil {
		t.Fatal("a corrupt record was written over by an answer")
	}
	if forgot, err := s.ForgetHomeSealed("fp1"); !forgot || err != nil {
		t.Fatalf("forget on a corrupt record: %v %v", forgot, err)
	}
	if m, err := s.HomeSealed("fp1"); m != "" || err != nil {
		t.Fatalf("after forget: %q %v", m, err)
	}
	if _, err := os.Stat(path + ".corrupt"); err == nil {
		t.Fatal("the moved-aside copy stayed")
	}
	// An empty file is corrupt too.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HomeSealed("fp1"); err == nil {
		t.Fatal("an empty record was read as none")
	}
}
