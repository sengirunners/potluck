// Package database owns project database recipes and snapshot provenance.
package database

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/sengirunners/potluck/protocol"
)

const Format = "potluck-postgres-snapshot-v1"

var digestRE = regexp.MustCompile(`^[a-f0-9]{64}$`)
var commitRE = regexp.MustCompile(`^[a-f0-9]{40}$`)
var imageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9./_:-]*@sha256:[a-f0-9]{64}$`)
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Manifest describes a stopped full PGDATA and its runtime in an immutable image.
type Manifest struct {
	PGDataPath  string `json:"pgdata_path"`
	ConfigFiles struct {
		Config  string   `json:"config"`
		HBA     string   `json:"hba"`
		Ident   string   `json:"ident"`
		Sources []string `json:"sources"`
	} `json:"config_files"`
	Format          string            `json:"format"`
	Project         string            `json:"project"`
	SourceCommit    string            `json:"source_commit"`
	InputsSHA256    string            `json:"inputs_sha256"`
	SnapshotSHA256  string            `json:"snapshot_sha256"`
	ConfigSHA256    string            `json:"config_sha256"`
	PostgresImage   string            `json:"postgres_image"`
	AgentImage      string            `json:"agent_image"`
	Arch            string            `json:"arch"`
	PostgresVersion string            `json:"postgres_version"`
	ClusterState    string            `json:"cluster_state"`
	MigrationCount  int               `json:"migration_count"`
	Extensions      map[string]string `json:"extensions"`
	Inputs          map[string]string `json:"inputs"`
	Runtime         map[string]string `json:"runtime"`
}

// ManifestMax bounds a recipe's manifest. A driver echoes the recipe in every
// acquire answer, and a remote host's answer is capped at 3 MiB once encoded
// (runtarget's opAnswerMax), where escaping can grow each byte up to 6.
const ManifestMax = 256 << 10

// Check validates a recipe and its manifest, naming the first rule it breaks.
func Check(d *protocol.Database) (Manifest, error) {
	var m Manifest
	if d == nil {
		return m, fmt.Errorf("missing database recipe")
	}
	if d.Project == "" || len(d.Project) > 256 || !nameRE.MatchString(d.Name) || !imageRE.MatchString(d.Image) {
		return m, fmt.Errorf("database requires project, recipe name and immutable image")
	}
	if len(d.Manifest) > ManifestMax {
		return m, fmt.Errorf("database manifest is over %d KiB", ManifestMax>>10)
	}
	if err := json.Unmarshal([]byte(d.Manifest), &m); err != nil {
		return m, fmt.Errorf("invalid database manifest: %v", err)
	}
	for _, c := range []struct {
		ok   bool
		what string
	}{
		{m.Format == Format, "format must be " + Format},
		{m.Project != "", "project is missing"},
		{commitRE.MatchString(m.SourceCommit), "source_commit must be a 40-hex commit"},
		{digestRE.MatchString(m.InputsSHA256), "inputs_sha256 must be a sha256"},
		{digestRE.MatchString(m.SnapshotSHA256), "snapshot_sha256 must be a sha256"},
		{digestRE.MatchString(m.ConfigSHA256), "config_sha256 must be a sha256"},
		{imageRE.MatchString(m.PostgresImage), "postgres_image must be pinned by digest"},
		{imageRE.MatchString(m.AgentImage), "agent_image must be pinned by digest"},
		{m.Arch == "amd64" || m.Arch == "arm64", "arch must be amd64 or arm64"},
		{m.ClusterState == "shut down", `cluster_state must be "shut down"`},
		{strings.HasPrefix(m.PostgresVersion, "17."), "postgres_version must be 17.x"},
		{m.MigrationCount >= 1, "migration_count must be at least 1"},
		{m.Extensions["pgtap"] != "", "extensions must list pgtap"},
		{len(m.Inputs) > 0, "inputs is empty"},
	} {
		if !c.ok {
			return m, fmt.Errorf("unsupported database manifest: %s", c.what)
		}
	}
	if !strings.HasPrefix(m.PGDataPath, "/") || m.PGDataPath == "/" || path.Clean(m.PGDataPath) != m.PGDataPath || strings.ContainsAny(m.PGDataPath, "\x00\n\r") {
		return m, fmt.Errorf("invalid original PGDATA path")
	}
	for _, p := range append([]string{m.ConfigFiles.Config, m.ConfigFiles.HBA, m.ConfigFiles.Ident}, m.ConfigFiles.Sources...) {
		if path.Clean(p) != p || !(strings.HasPrefix(p, m.PGDataPath+"/") || strings.HasPrefix(p, "/etc/postgresql/")) {
			return m, fmt.Errorf("database configuration outside packaged roots")
		}
	}
	for _, hash := range m.Inputs {
		if !digestRE.MatchString(hash) {
			return m, fmt.Errorf("invalid database input checksum")
		}
	}
	for _, key := range []string{"bindir", "libdir", "pkglibdir", "sharedir", "sysconfdir"} {
		p := m.Runtime[key]
		if p == "" || strings.Contains(p, "..") || strings.ContainsAny(p, "\x00\n\r") || !(strings.HasPrefix(p, "/nix/") || strings.HasPrefix(p, "/usr/lib/postgresql/") || p == "/etc/postgresql" || strings.HasPrefix(p, "/etc/postgresql/")) {
			return m, fmt.Errorf("unsupported database runtime path")
		}
	}
	return m, nil
}

// Digest binds the exact manifest bytes packaged in the image.
func Digest(manifest string) string {
	h := sha256.Sum256([]byte(manifest))
	return hex.EncodeToString(h[:])
}
