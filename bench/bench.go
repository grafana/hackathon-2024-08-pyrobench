package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"golang.org/x/perf/benchfmt"
	"golang.org/x/perf/benchmath"
	"golang.org/x/perf/benchproc"

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

type Benchmark struct {
	logger log.Logger

	baseDir      string
	baseCommit   string
	basePackages []Package
	headDir      string
	headCommit   string
	headPackages []Package

	statBuilders map[string]*StatBuilder
}

type BenchmarkResult struct {
	Ref       string           `json:"ref"`
	Type      string           `json:"type"`
	Benchmark *benchmarkResult `json:"benchmark"`
}

func New(logger log.Logger) (*Benchmark, error) {
	b := &Benchmark{
		logger:       logger,
		statBuilders: make(map[string]*StatBuilder),
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

func (b *Benchmark) addBenchStatResults(res *benchmarkResult, src benchSource) {
	builder, ok := b.statBuilders[res.Name]
	if !ok {
		var err error
		builder, err = NewStatBuilder()
		if err != nil {
			level.Error(b.logger).Log("msg", "error creating stat builder", "err", err)
			return
		}
		b.statBuilders[res.Name] = builder
	}

	if builder.Units == nil {
		builder.Units = res.Units
	}

	for _, r := range res.RawResult {
		ok, err := builder.Filter.Apply(r)
		if !ok && err != nil {
			// Non-fatal error, let's just skip this result.
			level.Error(b.logger).Log("msg", "error applying filter", "err", err)
			continue
		}

		r.SetConfig("source", src.String())
		builder.Stats.Add(r)
	}
}

func git(args ...string) ([]byte, error) {
	bufOut := new(bytes.Buffer)
	bufErr := new(bytes.Buffer)
	cmd := append([]string{"git"}, args...)
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdout = bufOut
	c.Stderr = bufErr

	err := c.Run()
	if err != nil {
		return nil, fmt.Errorf("command %v: %w\n%s", cmd, err, bufErr.String())
	}
	return bufOut.Bytes(), nil
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

	tables  *benchtab.Tables
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

// note(bryan): Only pass tables on the last call to generateReport.
func (b *Benchmark) generateReport(benchmarkGroups [][]*benchWithKey) *report.BenchmarkReport {
	rpt := &report.BenchmarkReport{
		BaseRef: b.baseCommit,
		HeadRef: b.headCommit,
	}

	for _, results := range benchmarkGroups {
		for _, res := range results {
			for i, r := range res.bench.results {

				switch r.Unit {
				case "ns":
					const unit = "sec/op"
					table := getTableForUnit(res.tables, unit)
					if table == nil {
						continue
					}

					if !strings.HasSuffix(res.bench.results[i].Name, ")") {
						res.bench.results[i].Name = fmt.Sprintf("%s (%s)", r.Name, unit)
					}
					for _, col := range table.Cols {
						switch col.String() {
						case "source:base":
							res.bench.results[i].BaseValue.ProfileValue = int64(table.Summary[col].Summary * 1e9)
						case "source:head":
							res.bench.results[i].HeadValue.ProfileValue = int64(table.Summary[col].Summary * 1e9)
						}
					}

					// TODO(bryan): Add diff value to report.BenchmarkResult do
					// use benchstat's diff value instead of calculating our
					// own.
				case "bytes":
					const unit = "B/op"
					table := getTableForUnit(res.tables, "B/op")
					if table == nil {
						continue
					}

					if !strings.HasSuffix(res.bench.results[i].Name, ")") {
						res.bench.results[i].Name = fmt.Sprintf("%s (%s)", r.Name, unit)
					}
					for _, col := range table.Cols {
						switch col.String() {
						case "source:base":
							res.bench.results[i].BaseValue.ProfileValue = int64(table.Summary[col].Summary)
						case "source:head":
							res.bench.results[i].HeadValue.ProfileValue = int64(table.Summary[col].Summary)
						}
					}
				case "": // allocs
					const unit = "allocs/op"
					table := getTableForUnit(res.tables, unit)
					if table == nil {
						continue
					}

					if !strings.HasSuffix(res.bench.results[i].Name, ")") {
						res.bench.results[i].Name = fmt.Sprintf("%s (%s)", r.Name, unit)
					}
					for _, col := range table.Cols {
						switch col.String() {
						case "source:base":
							res.bench.results[i].BaseValue.ProfileValue = int64(table.Summary[col].Summary)
						case "source:head":
							res.bench.results[i].HeadValue.ProfileValue = int64(table.Summary[col].Summary)
						}
					}
				}
			}

			rpt.Runs = append(rpt.Runs, report.BenchmarkRun{
				Name:            fmt.Sprintf("%s.%s", res.key.packagePath, res.key.benchmark),
				Reason:          res.bench.reason,
				Results:         res.bench.results,
				BenchStatTables: res.tables,
			})
		}
	}
	return rpt
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

		if res.base != nil && res.head != nil && len(res.base.testBinaryHash) > 0 && len(res.head.testBinaryHash) > 0 {
			// compare hash
			if bytes.Equal(res.base.testBinaryHash, res.head.testBinaryHash) {
				continue
			}
		}

		if res.base == nil {
			res.reason = "benchmark does not exist in base"
		} else if res.head == nil {
			res.reason = "benchmark does not exist in head"
		} else if len(res.base.testBinaryHash) > 0 && len(res.head.testBinaryHash) > 0 {
			res.reason = "code changed"
		} else {
			res.reason = "tbd"
		}

		benchmarkToBeRun = append(
			benchmarkToBeRun,
			&benchWithKey{
				key:   k,
				bench: res,
			},
		)
	}

	return benchmarkToBeRun
}

func getTableForUnit(tables *benchtab.Tables, unit string) *benchtab.Table {
	for _, table := range tables.Tables {
		if table.Unit == unit {
			return table
		}
	}
	return nil
}

func resultFromPackages(f func(benchKey, *Package), pkgs []Package) {
	for idx := range pkgs {
		p := &pkgs[idx]
		for _, b := range p.benchmarkNames {
			f(benchKey{p.meta.ImportPath, b.Name}, p)
		}
	}
}

type StatBuilder struct {
	Stats  *benchtab.Builder
	Filter *benchproc.Filter
	Units  benchfmt.UnitMetadataMap
}

func (sb *StatBuilder) ToTables() *benchtab.Tables {
	return sb.Stats.ToTables(benchtab.TableOpts{
		Confidence: 0.95,
		Thresholds: &benchmath.DefaultThresholds,
		Units:      sb.Units,
	})
}

func NewStatBuilder() (*StatBuilder, error) {
	stat, filter, err := benchtab.NewDefaultBuilder()
	if err != nil {
		return nil, err
	}

	sb := &StatBuilder{
		Stats:  stat,
		Filter: filter,
	}
	return sb, nil
}
