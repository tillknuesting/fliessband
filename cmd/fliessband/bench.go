package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tillknuesting/fliessband/internal/bench"
	"github.com/tillknuesting/fliessband/internal/llm"
	"github.com/tillknuesting/fliessband/internal/pipeline"
)

// benchJob is one spec run with one model.
type benchJob struct {
	spec, model string // labels in the results
	src         []byte
	repeat      int
	opts        pipeline.Options
}

// benchCmd runs every spec with every model, repeat times, through the
// line and writes the results; with -baseline it fails on a regression.
func benchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	specs := fs.String("specs", "specs/*.yaml", "glob of specs to run")
	models := fs.String("models", "", `comma-separated models; "template" means no LLM (default: template only, or the configured LLM with -llm)`)
	useLLM := fs.Bool("llm", false, "with no -models, run the configured LLM (FLIESSBAND_LLM_MODEL or FLIESSBAND_LLM_MODELS)")
	repeat := fs.Int("repeat", 1, "runs per spec and model")
	parallel := fs.Int("parallel", 2, "runs executed at the same time")
	out := fs.String("out", "data/bench/results.json", "where to write the results JSON")
	baseline := fs.String("baseline", "", "results JSON of an earlier benchmark to compare against")
	maxRegression := fs.Float64("max-regression", 3, "score points the mean score of a spec and model may drop against the baseline")
	work := fs.String("data", "data/bench", "directory for the benchmark's runs")
	keep := fs.Bool("keep", false, "keep the run directories (generated services) instead of deleting them")
	maxMutants := fs.Int("max-mutants", 0, "mutants to run at most per run (default 150)")
	fuzzTime := fs.String("fuzztime", "3s", "fuzzing time per target")
	_ = fs.Parse(args)
	if fs.NArg() != 0 || *repeat < 1 || *parallel < 1 {
		usage()
	}

	var base *bench.Results
	if *baseline != "" { // read it first, so a bad path fails before an hour of runs
		var err error
		if base, err = bench.Load(*baseline); err != nil {
			return err
		}
	}
	paths, err := filepath.Glob(*specs)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no specs match %s", *specs)
	}
	slices.Sort(paths)

	client := llm.FromEnv()
	type variant struct {
		label string
		opts  pipeline.Options
	}
	defaults := pipeline.DefaultOptions()
	defaults.MaxMutants, defaults.FuzzTime = *maxMutants, *fuzzTime
	var variants []variant
	switch list := llm.SplitModels(*models); {
	case len(list) > 0:
		for _, m := range list {
			o := defaults
			if m != bench.TemplateModel {
				o.LLM, o.Model = true, m
			}
			variants = append(variants, variant{m, o})
		}
	case *useLLM:
		if client == nil {
			return errors.New("-llm needs an LLM: set FLIESSBAND_LLM_API_KEY or OPENAI_API_KEY")
		}
		o := defaults
		o.LLM = true
		variants = append(variants, variant{strings.Join(client.Candidates(), "+"), o})
	default:
		variants = append(variants, variant{bench.TemplateModel, defaults})
	}
	if client == nil && slices.ContainsFunc(variants, func(v variant) bool { return v.opts.LLM }) {
		return errors.New("-models names an LLM but none is configured: set FLIESSBAND_LLM_API_KEY or OPENAI_API_KEY")
	}

	var jobs []benchJob
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		for _, v := range variants {
			for i := 1; i <= *repeat; i++ {
				jobs = append(jobs, benchJob{spec: name, model: v.label, src: src, repeat: i, opts: v.opts})
			}
		}
	}

	// Every run gets its own data directory, so no run sees another as the
	// baseline of its compatibility check and repeats stay comparable.
	workDir := filepath.Join(*work, time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return err
	}
	if !*keep {
		defer func() { _ = os.RemoveAll(workDir) }()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "bench: %d specs × %d models × %d repeats = %d runs, %d at a time\n",
		len(paths), len(variants), *repeat, len(jobs), *parallel)
	start := time.Now()
	rows := make([]bench.Row, len(jobs))
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		done int
	)
	sem := make(chan struct{}, *parallel)
	for i, j := range jobs {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				rows[i] = bench.Row{Spec: j.spec, Model: j.model, Repeat: j.repeat, Status: string(pipeline.Canceled)}
				return
			}
			defer func() { <-sem }()
			rows[i] = benchRun(ctx, client, filepath.Join(workDir, fmt.Sprint(i)), j, *keep)
			mu.Lock()
			done++
			r := rows[i]
			score := "–"
			if r.Score != nil {
				score = fmt.Sprintf("%.1f", *r.Score)
			}
			fmt.Fprintf(os.Stderr, "[%d/%d] %s %s #%d: %s, score %s, %.0fs\n", done, len(jobs), j.spec, j.model, j.repeat,
				strings.TrimSpace(r.Status+" "+r.FailedAt+" "+r.Error), score, r.DurationMs/1000)
			mu.Unlock()
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return errors.New("benchmark interrupted; no results written")
	}

	res := bench.New(rows, *repeat)
	res.CreatedAt = time.Now().UTC().Truncate(time.Second)
	res.Factory, res.GoVersion, res.Platform = pipeline.Version, runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o750); err != nil {
			return err
		}
		if err := res.Save(*out); err != nil {
			return err
		}
	}
	fmt.Print(res.Markdown())
	fmt.Printf("\n%d runs in %s", len(jobs), time.Since(start).Round(time.Second))
	if *out != "" {
		fmt.Printf(", results in %s", *out)
	}
	fmt.Println()
	if base != nil {
		c := bench.Compare(base, res, *maxRegression)
		fmt.Print("\n" + c.Report())
		if !c.OK() {
			return fmt.Errorf("%d regression(s) against %s", len(c.Regressions), *baseline)
		}
	}
	return nil
}

// benchRun runs one job through the line in its own data directory and
// turns the finished run into a row.
func benchRun(ctx context.Context, client *llm.Client, dataDir string, j benchJob, keep bool) bench.Row {
	row := bench.Row{Spec: j.spec, Model: j.model, Repeat: j.repeat}
	fail := func(err error) bench.Row {
		row.Status, row.Error = "error", err.Error()
		return row
	}
	m, err := pipeline.NewManager(dataDir, toolsDir(), client)
	if err != nil {
		return fail(err)
	}
	m.SetLimits(1, 0, 1)
	r, err := m.Start(j.src, j.opts)
	if err != nil {
		return fail(err)
	}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			m.Cancel(r.ID)
		case <-stopped:
		}
	}()
	m.Wait(r.ID)
	close(stopped)
	var final pipeline.Run
	if err := json.Unmarshal(r.JSON(), &final); err != nil {
		return fail(err)
	}
	row = bench.FromRun(j.spec, j.model, j.repeat, &final)
	if !keep {
		_ = os.RemoveAll(dataDir)
	}
	return row
}
