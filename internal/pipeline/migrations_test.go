package pipeline

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/metrics"
)

const migV1 = `name: catalog
package: acme.catalog.v1
description: a catalog
entities:
  - name: Item
    fields:
      - {name: title, type: string, required: true}
      - {name: done, type: bool}
`

// migrationRun runs the spec, scaffold and migrations stations of a new
// run created at the given time. They need no external tools.
func migrationRun(t *testing.T, m *Manager, id, src string, at time.Time, opts Options) (*Run, *StageState) {
	t.Helper()
	r := newRun(id, []byte(src), opts, filepath.Join(m.dataDir, "runs", id))
	r.CreatedAt = at
	m.mu.Lock()
	m.runs[id] = r // known to the manager, as runs it starts are
	m.mu.Unlock()
	if err := os.MkdirAll(r.ServiceDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	x := &executor{m: m, r: r, src: []byte(src), rep: &metrics.Report{Factory: metrics.Factory{StageMs: map[string]float64{}}}}
	ctx := t.Context()
	if !x.do(ctx, "spec", x.stageSpec) || !x.do(ctx, "scaffold", x.stageScaffold) {
		t.Fatalf("%s: spec or scaffold failed: %+v", id, r.Stages[:2])
	}
	x.do(ctx, "migrations", x.stageMigrations)
	r.Report = x.rep
	return r, r.stage("migrations")
}

// pass marks a run as passed on disk, so later runs find it as a baseline.
func pass(t *testing.T, r *Run) {
	t.Helper()
	r.Status = Passed
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
}

func migrationNames(t *testing.T, r *Run) []string {
	t.Helper()
	names, err := migrationFiles(filepath.Join(r.ServiceDir(), filepath.FromSlash(gen.MigrationsDir)))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func readMigration(t *testing.T, r *Run, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.ServiceDir(), filepath.FromSlash(gen.MigrationsDir), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMigrationsAcrossVersions takes a service through four versions: the
// first creates the schema, the second adds to it, the third drops a column
// and is stopped, and the fourth allows the drop.
func TestMigrationsAcrossVersions(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)

	v1, st := migrationRun(t, m, "r1", migV1, t0, Options{})
	if st.Status != Skipped || !strings.Contains(st.Summary, "first version") {
		t.Fatalf("v1 migrations = %s %q", st.Status, st.Summary)
	}
	if got := migrationNames(t, v1); !slices.Equal(got, []string{"0001_init.sql"}) {
		t.Fatalf("v1 files = %v", got)
	}
	init := readMigration(t, v1, "0001_init.sql")
	pass(t, v1)

	v2src := strings.Replace(migV1, "      - {name: done, type: bool}\n",
		"      - {name: done, type: bool}\n      - {name: notes, type: string, max_len: 500}\n", 1) +
		"  - name: Label\n    fields:\n      - {name: name, type: string, unique: true}\n"
	v2, st := migrationRun(t, m, "r2", v2src, t0.Add(time.Minute), Options{})
	if st.Status != Passed {
		t.Fatalf("v2 migrations = %s %q\n%s", st.Status, st.Summary, strings.Join(st.Log, "\n"))
	}
	files := migrationNames(t, v2)
	if len(files) != 2 || files[0] != "0001_init.sql" || !strings.HasPrefix(files[1], "0002_add_labels") {
		t.Fatalf("v2 files = %v", files)
	}
	if got := readMigration(t, v2, "0001_init.sql"); got != init {
		t.Error("0001_init.sql was not carried forward verbatim")
	}
	if strings.Contains(init, "labels") {
		t.Fatal("v1's schema already has labels")
	}
	if sql := readMigration(t, v2, files[1]); !strings.Contains(sql, `CREATE TABLE "labels"`) || !strings.Contains(sql, `ADD COLUMN "notes"`) {
		t.Errorf("0002 lacks the changes:\n%s", sql)
	}
	mig := v2.Report.Migrations
	if mig == nil || mig.Baseline != "r1" || mig.Generated != files[1] || !slices.Equal(mig.Files, files) || len(mig.Changes) != 2 || mig.Blocked {
		t.Errorf("v2 report = %+v", mig)
	}
	man, err := gen.LoadManifest(v2.ServiceDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if man.Origin(gen.MigrationsDir+"/"+f) != gen.OriginTemplate {
			t.Errorf("%s missing from the manifest", f)
		}
	}
	pass(t, v2)

	v3src := strings.Replace(v2src, "      - {name: done, type: bool}\n", "", 1)
	v3, st := migrationRun(t, m, "r3", v3src, t0.Add(2*time.Minute), Options{})
	if st.Status != Failed || !strings.Contains(st.Summary, "1 destructive schema change against run r2") ||
		!strings.Contains(st.Summary, `drop column "items"."done"`) || !strings.Contains(st.Summary, "allow_destructive") {
		t.Fatalf("v3 migrations = %s %q", st.Status, st.Summary)
	}
	if got := migrationNames(t, v3); !slices.Equal(got, files) {
		t.Errorf("a blocked run wrote a migration: %v", got)
	}
	if mig := v3.Report.Migrations; mig == nil || !mig.Blocked || len(mig.Changes) != 1 || !mig.Changes[0].Destructive {
		t.Errorf("v3 report = %+v", mig)
	}

	v4, st := migrationRun(t, m, "r4", v3src+"migrations: {allow_destructive: true}\n", t0.Add(3*time.Minute), Options{})
	if st.Status != Warning || !strings.Contains(st.Summary, "0003_drop_items_done.sql") {
		t.Fatalf("v4 migrations = %s %q", st.Status, st.Summary)
	}
	if sql := readMigration(t, v4, "0003_drop_items_done.sql"); !strings.Contains(sql, `ALTER TABLE "items" DROP COLUMN "done";`) {
		t.Errorf("0003:\n%s", sql)
	}
	if mig := v4.Report.Migrations; mig == nil || !mig.Allowed || mig.Baseline != "r2" || len(mig.Files) != 3 {
		t.Errorf("v4 report = %+v", mig)
	}

	// Improving on a passed version migrates from that version, not the
	// newest one; a failed base is not used.
	v5, _ := migrationRun(t, m, "r5", migV1, t0.Add(4*time.Minute), Options{Base: "r1"})
	if mig := v5.Report.Migrations; mig == nil || mig.Baseline != "r1" || len(mig.Changes) != 0 {
		t.Errorf("v5 report = %+v", mig)
	}
	v6, st := migrationRun(t, m, "r6", v2src, t0.Add(5*time.Minute), Options{Base: "r3"})
	if mig := v6.Report.Migrations; mig == nil || mig.Baseline != "r2" || !strings.Contains(strings.Join(st.Log, "\n"), "did not pass") {
		t.Errorf("v6 report = %+v", mig)
	}
}

func TestMigrationsSkipWithoutPostgres(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, st := migrationRun(t, m, "r1", migV1+"features: {postgres: false}\n", time.Now(), Options{})
	if st.Status != Skipped || !strings.Contains(st.Summary, "no PostgreSQL store") {
		t.Errorf("migrations = %s %q", st.Status, st.Summary)
	}
}
