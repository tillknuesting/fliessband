package metrics

// Migrations reports the database schema migration of a run: the files
// carried forward from the previous version, the one generated for this
// spec, and every schema change with its risk.
type Migrations struct {
	Baseline  string            `json:"baseline,omitempty"`  // run whose migrations were carried forward
	Files     []string          `json:"files"`               // the full chain, in the order it is applied
	Generated string            `json:"generated,omitempty"` // the file this run added
	Changes   []MigrationChange `json:"changes"`
	// Blocked is set when destructive changes stopped the line; Allowed when
	// the spec let them through with migrations.allow_destructive.
	Blocked bool   `json:"blocked,omitempty"`
	Allowed bool   `json:"allowed,omitempty"`
	Note    string `json:"note,omitempty"`
}

// MigrationChange is one schema change between two versions.
type MigrationChange struct {
	Kind        string `json:"kind"`
	Table       string `json:"table"`
	Column      string `json:"column,omitempty"`
	Detail      string `json:"detail"`
	Destructive bool   `json:"destructive"`
}
