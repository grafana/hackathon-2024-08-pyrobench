package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"golang.org/x/perf/benchfmt"
	"golang.org/x/perf/benchmath"
	"golang.org/x/perf/benchproc"
	"golang.org/x/sync/errgroup"

	"github.com/grafana/pyrobench/benchtab"
	"github.com/grafana/pyrobench/report"
)

type contextKey uint8

const (
	contextKeyCleanup contextKey = iota
)

type cleaner struct {
	mtx      sync.Mutex
	cleanups []func() error
}

func (c *cleaner) add(f func() error) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	c.cleanups = append(c.cleanups, f)
}

func (c *cleaner) cleanup() error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	var err error
	l := len(c.cleanups)
	// cleanup in reverse order
	for idx := range c.cleanups {
		err = errors.Join(err, c.cleanups[l-1-idx]())
	}
	return err
}

type cleanupHookFunc func(func() error)

func addCleanupToContext(ctx context.Context, f cleanupHookFunc) context.Context {
	return context.WithValue(ctx, contextKeyCleanup, f)
}

func cleanupFromContext(ctx context.Context) cleanupHookFunc {
	cleanup, ok := ctx.Value(contextKeyCleanup).(cleanupHookFunc)
	if !ok {
		panic("cleanup hook not found in context")
	}
	return cleanup
}

type CompareArgs struct {
	GitBase    string
	BenchTime  string
	BenchCount uint16
	Report     *report.Args
}

func AddCompareCommand(app *kingpin.Application) (*kingpin.CmdClause, *CompareArgs) {
	cmd := app.Command("compare", "Compare Golang Mirco Benchmarks using CPU/Memory profiles.")
	reportParams := report.AddArgs(cmd)
	args := CompareArgs{
		Report: reportParams,
	}
	cmd.Flag("git-base", "Git base commit").Default("HEAD~1").StringVar(&args.GitBase)
	cmd.Flag("bench-time", "Golang's benchtime argument.").Default("2s").StringVar(&args.BenchTime)
	cmd.Flag("bench-count", "Golang's count argument. How often to repeat the benchmarks").Default("5").Uint16Var(&args.BenchCount)
	return cmd, &args
}

type Benchmark struct {
	logger log.Logger

	baseDir      string
	baseCommit   string
	basePackages []Package
	headDir      string
	headCommit   string
	headPackages []Package

	statBuilder *benchtab.Builder
	statFilter  *benchproc.Filter
	statUnits   benchfmt.UnitMetadataMap
}

type BenchmarkResult struct {
	Ref       string           `json:"ref"`
	Type      string           `json:"type"`
	Benchmark *benchmarkResult `json:"benchmark"`
}

func New(logger log.Logger) (*Benchmark, error) {
	stat, filter, err := benchtab.NewDefaultBuilder()
	if err != nil {
		return nil, err
	}

	b := &Benchmark{
		logger:      logger,
		statBuilder: stat,
		statFilter:  filter,
	}
	return b, nil
}

func (b *Benchmark) prerequisites(_ context.Context) error {
	return errors.Join(
		func() error {
			_, err := exec.LookPath("go")
			return err
		}(),
		func() error {
			_, err := exec.LookPath("git")
			return err
		}(),
	)
}

func (b *Benchmark) gitRevParse(ctx context.Context, rev string) (string, error) {
	c, err := exec.CommandContext(ctx, "git", "rev-parse", rev).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(c)), nil
}

func (b *Benchmark) addBenchStatResults(results []*benchfmt.Result, units benchfmt.UnitMetadataMap, src benchSource) {
	if b.statUnits == nil {
		b.statUnits = units
	}

	for _, r := range results {
		ok, err := b.statFilter.Apply(r)
		if !ok && err != nil {
			// Non-fatal error, let's just skip this result.
			level.Error(b.logger).Log("msg", "error applying filter", "err", err)
			continue
		}

		r.SetConfig("source", src.String())
		b.statBuilder.Add(r)
	}
}

func (b *Benchmark) benchStatTable() *benchtab.Tables {
	return b.statBuilder.ToTables(benchtab.TableOpts{
		Confidence: 0.95,
		Thresholds: &benchmath.DefaultThresholds,
		Units:      b.statUnits,
	})
}

func (b *Benchmark) gitCheckoutBase(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "pyrobench-base")
	if err != nil {
		return err
	}
	b.baseDir = dir

	err = exec.CommandContext(ctx, "git", "worktree", "add", b.baseDir, b.baseCommit).Run()
	if err != nil {
		return err
	}
	cleanupFromContext(ctx)(func() error {
		err := exec.Command("git", "worktree", "remove", b.baseDir).Run()
		if err != nil {
			return fmt.Errorf("failed to cleanup git workdir: %w", err)
		}
		return nil
	})
	return nil
}

func countPackagesWithTests(packages []Package) int {
	count := 0
	for _, p := range packages {
		if len(p.meta.TestGoFiles) > 0 {
			count++
		}
	}
	return count
}

func (b *Benchmark) Compare(ctx context.Context, args *CompareArgs) error {
	cleaner := &cleaner{}
	ctx = addCleanupToContext(ctx, cleaner.add)
	defer func() {
		err := cleaner.cleanup()
		if err != nil {
			level.Error(b.logger).Log("msg", "error cleaning up", "err", err)
		}
	}()

	err := b.prerequisites(ctx)
	if err != nil {
		return fmt.Errorf("error checking prerequisites: %w", err)
	}

	// initialize reporter
	updateCh := make(chan *report.BenchmarkReport)
	reporter, err := report.New(b.logger, args.Report, updateCh)
	if err != nil {
		return fmt.Errorf("error initializing reporter: %w", err)
	}
	defer reporter.Stop()

	// resolve base commit
	b.baseCommit, err = b.gitRevParse(ctx, args.GitBase)
	if err != nil {
		return fmt.Errorf("error resolving base git rev: %w", err)
	}
	b.headCommit, err = b.gitRevParse(ctx, "HEAD")
	if err != nil {
		return fmt.Errorf("error resolving head git rev: %w", err)
	}
	level.Info(b.logger).Log("msg", "comparing commits", "base", b.baseCommit, "head", b.headCommit)

	// get working directory
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("error getting working directory: %w", err)
	}
	b.headDir, err = filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("error getting absolute path of working directory: %w", err)
	}

	// checkout base commit
	err = b.gitCheckoutBase(ctx)
	if err != nil {
		return fmt.Errorf("error checking out base commit %s: %w", b.baseCommit, err)
	}

	headPackages, err := discoverPackages(ctx, b.logger, b.headDir)
	if err != nil {
		return fmt.Errorf("error discovering packages in head: %w", err)
	}
	b.headPackages = headPackages

	basePackages, err := discoverPackages(ctx, b.logger, b.baseDir)
	if err != nil {
		return fmt.Errorf("error discovering packages in head: %w", err)
	}
	b.basePackages = basePackages

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)

	level.Info(b.logger).Log("msg", "compiling packages with tests to figure out what changed", "base", countPackagesWithTests(basePackages), "head", countPackagesWithTests(headPackages))
	for _, pkgs := range [][]Package{b.basePackages, b.headPackages} {
		for idx := range pkgs {
			p := &pkgs[idx]
			g.Go(func() error {
				err := p.compileTest(gctx)
				if err != nil {
					return err
				}

				return p.listBenchmarks(gctx)
			})
		}
	}

	err = g.Wait()
	if err != nil {
		return err
	}

	benchmarks := b.compareResult()
	if len(benchmarks) == 0 {
		level.Info(b.logger).Log("msg", "no benchmarks to run")
		return nil
	}

	updateCh <- b.generateReport(benchmarks)
	for _, r := range benchmarks {
		if r.base != nil {
			res, err := r.bench.base.runBenchmark(ctx, args, r.key.benchmark)
			if err != nil {
				level.Error(b.logger).Log("msg", "error running benchmark", "package", r.base.meta.ImportPath, "benchmark", r.key.benchmark, "err", err)
			}

			b.addBenchStatResults(res.RawResult, res.Units, benchSourceBase)
			r.addResult(benchSourceBase, res)
			updateCh <- b.generateReport(benchmarks)
		}
		if r.head != nil {
			res, err := r.bench.head.runBenchmark(ctx, args, r.key.benchmark)
			if err != nil {
				level.Error(b.logger).Log("msg", "error running benchmark", "package", r.base.meta.ImportPath, "benchmark", r.key.benchmark, "err", err)
			}

			b.addBenchStatResults(res.RawResult, res.Units, benchSourceHead)
			r.addResult(benchSourceHead, res)
			updateCh <- b.generateReport(benchmarks)
		}
	}

	tables := b.benchStatTable()
	tables.ToText(os.Stdout, false)

	return nil
}

type benchKey struct {
	packagePath string
	benchmark   string
}

type benchWithKey struct {
	*bench
	key benchKey
}

type bench struct {
	base   *Package
	head   *Package
	reason string

	results []report.BenchmarkResult
}

type benchSource uint8

const (
	benchSourceUnknown benchSource = iota
	benchSourceHead
	benchSourceBase
)

func (b benchSource) String() string {
	switch b {
	case benchSourceBase:
		return "base"
	case benchSourceHead:
		return "head"
	default:
		return "unknown"
	}
}

func (b *bench) addResult(source benchSource, res *benchmarkResult) {
	m := map[string]struct {
		unit string
		res  *profileResult
	}{
		"cpu":           {"ns", &res.CPU},
		"alloc_space":   {"bytes", &res.AllocSpace},
		"alloc_objects": {"", &res.AllocObjects},
	}

	addValue := func(xres *report.BenchmarkResult, xprof *profileResult) {
		v := report.BenchmarkValue{
			ProfileValue:  xprof.Total,
			FlamegraphKey: xprof.Key,
		}
		if source == benchSourceBase {
			xres.BaseValue = v

		} else if source == benchSourceHead {
			xres.HeadValue = v
		} else {
			panic("unknown source")
		}
	}

	for idx := range b.results {
		res := &b.results[idx]
		prof, ok := m[res.Name]
		if !ok {
			continue
		}

		if prof.res.Key == "" {
			continue
		}

		addValue(res, prof.res)

		delete(m, res.Name)
		// if not empty

	}

	for name, prof := range m {
		res := report.BenchmarkResult{
			Name: name,
			Unit: prof.unit,
		}
		addValue(&res, prof.res)
		b.results = append(b.results, res)
	}

	sort.Slice(b.results, func(i, j int) bool {
		return b.results[i].Name == b.results[j].Name
	})
}

type benchMap struct {
	m       map[benchKey]int
	results []bench
}

func newBenchMap(len int) *benchMap {
	return &benchMap{
		m:       make(map[benchKey]int, len),
		results: make([]bench, 0, len),
	}
}

func (r *benchMap) get(k benchKey) *bench {
	idx, ok := r.m[k]
	if !ok {
		idx = len(r.results)
		r.m[k] = idx
		r.results = append(r.results, bench{})
	}
	return &r.results[idx]
}

func (b *Benchmark) generateReport(results []*benchWithKey) *report.BenchmarkReport {
	r := &report.BenchmarkReport{
		BaseRef: b.baseCommit,
		HeadRef: b.headCommit,
	}
	r.Runs = make([]report.BenchmarkRun, 0, len(results))
	for _, res := range results {
		r.Runs = append(r.Runs, report.BenchmarkRun{
			Name:    fmt.Sprintf("%s.%s", res.key.packagePath, res.key.benchmark),
			Results: res.bench.results,
		})
	}
	return r
}

func (b *Benchmark) compareResult() []*benchWithKey {
	r := newBenchMap(len(b.headPackages))

	resultFromPackages(func(k benchKey, p *Package) {
		x := r.get(k)
		x.head = p
	}, b.headPackages)
	resultFromPackages(func(k benchKey, p *Package) {
		x := r.get(k)
		x.base = p
	}, b.basePackages)

	if len(r.results) == 0 {
		return nil
	}

	keys := make([]benchKey, len(r.m))
	for k, v := range r.m {
		keys[v] = k
	}

	benchmarkToBeRun := make([]*benchWithKey, 0, len(r.results))
	for idx := range r.results {
		res := &r.results[idx]
		k := keys[idx]

		if res.base != nil && res.head != nil {
			// compare hash
			if bytes.Equal(res.base.testBinaryHash, res.head.testBinaryHash) {
				continue
			}
		}

		if res.base == nil {
			res.reason = "benchmark does not exist in base"
		} else if res.head == nil {
			res.reason = "benchmark does not exist in head"
		} else {
			res.reason = "code change"
		}

		benchmarkToBeRun = append(
			benchmarkToBeRun,
			&benchWithKey{
				key:   k,
				bench: res,
			},
		)
		level.Debug(b.logger).Log("msg", "benchmark will be run", "package", res.head.meta.ImportPath, "benchmark", k.benchmark, "reason", res.reason)
	}

	return benchmarkToBeRun
}

func resultFromPackages(f func(benchKey, *Package), pkgs []Package) {
	for idx := range pkgs {
		p := &pkgs[idx]
		for _, b := range p.benchmarkNames {
			f(benchKey{p.meta.ImportPath, b}, p)
		}
	}
}
