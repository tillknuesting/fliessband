package pipeline

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/spec"
)

// stageMigrations keeps the database schema history of a service. The
// scaffold writes 0001_init.sql for the current spec, which is right only
// for a first version. When an earlier version exists, its migration files
// replace that file verbatim, so history is never rewritten, and the
// difference between its spec and this one becomes the next migration.
// Destructive changes fail the gate unless the spec allows them.
func (x *executor) stageMigrations(_ context.Context, st *StageState) error {
	if !x.model.Postgres {
		return skip("the service has no PostgreSQL store")
	}
	mig := &metrics.Migrations{Changes: []metrics.MigrationChange{}}
	x.rep.Migrations = mig
	dir := filepath.Join(x.r.ServiceDir(), filepath.FromSlash(gen.MigrationsDir))
	current, err := migrationFiles(dir)
	if err != nil {
		return err
	}
	mig.Files = current
	first := func(note string) error {
		mig.Note = note
		return skip("%s: %s creates the schema", note, strings.Join(current, ", "))
	}

	prev := x.migrationBase(st)
	if prev == nil {
		return first("first version of " + x.spec.Name)
	}
	prev.mu.Lock()
	prevSrc := prev.Spec
	prev.mu.Unlock()
	ps, problems, err := spec.Parse([]byte(prevSrc))
	if err != nil {
		return fmt.Errorf("the spec of run %s, the previous version, no longer parses, so the schema change cannot be computed: %w", prev.ID, err)
	}
	if spec.HasErrors(problems) {
		return fmt.Errorf("the spec of run %s, the previous version, no longer validates, so the schema change cannot be computed: %v", prev.ID, problems)
	}
	if !gen.NewModel(ps).Postgres {
		return first(fmt.Sprintf("run %s had no PostgreSQL store", prev.ID))
	}
	prevDir := filepath.Join(prev.ServiceDir(), filepath.FromSlash(gen.MigrationsDir))
	history, err := migrationFiles(prevDir)
	if err != nil || len(history) == 0 {
		return first(fmt.Sprintf("run %s has no migration files", prev.ID))
	}

	// Carry the history forward verbatim.
	for _, name := range current {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
		x.man.Remove(path.Join(gen.MigrationsDir, name))
	}
	for _, name := range history {
		b, err := os.ReadFile(filepath.Join(prevDir, name))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil { //nolint:gosec // part of the generated source tree
			return err
		}
		x.man.Set(path.Join(gen.MigrationsDir, name), gen.OriginTemplate, "carried from run "+prev.ID, b)
		x.log(st, fmt.Sprintf("carried forward %s from run %s", name, prev.ID))
	}
	mig.Baseline, mig.Files = prev.ID, history

	plan := gen.PlanMigration(ps, x.spec)
	var blocked []string
	for _, c := range plan.Changes {
		mig.Changes = append(mig.Changes, metrics.MigrationChange{Kind: c.Kind, Table: c.Table, Column: c.Column, Detail: c.Detail, Destructive: c.Destructive})
		mark := "safe"
		if c.Destructive {
			mark = "DESTRUCTIVE"
			blocked = append(blocked, c.Detail)
		}
		x.log(st, fmt.Sprintf("%-11s %s", mark, c.Detail))
	}
	if plan.Blocked() {
		mig.Blocked = true
		if err := x.man.Save(x.r.ServiceDir()); err != nil {
			return err
		}
		shown := blocked
		if len(shown) > 3 {
			shown = append(slices.Clone(shown[:3]), fmt.Sprintf("and %d more", len(blocked)-3))
		}
		return fmt.Errorf("%s against run %s could lose data or fail on existing rows: %s. Declare renames with renamed_from, "+
			"or set migrations: {allow_destructive: true} for this version",
			fmtCount(len(blocked), "destructive schema change", "destructive schema changes"), prev.ID, strings.Join(shown, "; "))
	}
	if len(plan.Changes) == 0 {
		x.summary(st, "schema unchanged since run %s; %s carried forward", prev.ID, fmtCount(len(history), "migration", "migrations"))
		return x.man.Save(x.r.ServiceDir())
	}

	f := gen.RenderMigration(x.spec, plan, history, "run "+prev.ID)
	if err := gen.WriteFiles(x.r.ServiceDir(), []gen.File{f}); err != nil {
		return err
	}
	x.man.Set(f.Path, gen.OriginTemplate, f.Template, f.Data)
	if err := x.man.Save(x.r.ServiceDir()); err != nil {
		return err
	}
	mig.Generated = path.Base(f.Path)
	mig.Files = append(slices.Clone(history), mig.Generated)
	x.log(st, "wrote "+f.Path)
	if len(blocked) > 0 {
		mig.Allowed = true
		return warn("%s with %s allowed by migrations.allow_destructive", mig.Generated,
			fmtCount(len(blocked), "destructive change", "destructive changes"))
	}
	x.summary(st, "%s: %s since run %s, all safe", mig.Generated, fmtCount(len(plan.Changes), "schema change", "schema changes"), prev.ID)
	return nil
}

// migrationBase is the version whose schema this run migrates from: the run
// it improves on when that run passed, else the compatibility baseline (the
// last passing run of the service).
func (x *executor) migrationBase(st *StageState) *Run {
	if id := x.r.Options.Base; id != "" {
		switch b := x.m.Get(id); {
		case b == nil:
			x.log(st, "base run "+id+" not found; migrating from the last passing run")
		case b.Name != x.spec.Name:
			x.log(st, "base run "+id+" is another service; migrating from the last passing run")
		case b.status() != Passed:
			x.log(st, "base run "+id+" did not pass, so its schema never shipped; migrating from the last passing run")
		default:
			return b
		}
	}
	return x.m.baseline(x.spec.Name, x.r.CreatedAt)
}

// migrationFiles lists the .sql files of a migrations directory in the
// order they are applied.
func migrationFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out, nil
}
