package protocol

// CapDatabase promises a private Postgres snapshot, verified before ready.
const CapDatabase = "database-snapshot-v1"

// DatabaseHome is the private database directory relative to guest HOME.
const DatabaseHome = ".potluck-database"

// DatabaseArtifactDir holds the immutable manifest and stopped snapshot.
const DatabaseArtifactDir = "/opt/potluck-db"

// Database is an authority-resolved project recipe. It contains provenance,
// never a connection credential. Drivers must compare the entire value on replay.
type Database struct {
	Project  string `json:"project"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	Manifest string `json:"manifest"`
}
