package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"golang.org/x/sync/errgroup"
)

type CompareArgs struct {
	GitBase string

	BenchTime string
}

func AddCompareCommand(app *kingpin.Application) (*kingpin.CmdClause, *CompareArgs) {
	cmd := app.Command("compare", "Compare Golang Mirco Benchmarks using CPU/Memory profiles.")

	args := CompareArgs{}
	cmd.Flag("git-base", "Git base commit").Default("HEAD~1").StringVar(&args.GitBase)
	cmd.Flag("bench-time", "Golang's benchtime argument.").Default("10s").StringVar(&args.BenchTime)
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
}

func New(logger log.Logger) *Benchmark {
	return &Benchmark{
		logger: logger,
	}
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

func (b *Benchmark) gitCheckoutBase(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "pyrobench-base")
	if err != nil {
		return err
	}
	b.baseDir = dir

	return exec.CommandContext(ctx, "git", "worktree", "add", b.baseDir, b.baseCommit).Run()
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
	err := b.prerequisites(ctx)
	if err != nil {
		return fmt.Errorf("error checking prerequisites: %w", err)
	}

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

	res := b.compareResult()
	if len(res) == 0 {
		level.Info(b.logger).Log("msg", "no benchmarks to run")
	}
	for _, r := range res {
		logFields := []interface{}{}
		handleResult := func(name string, res *benchmarkResult) {
			if res.CPUProfileKey != "" {
				logFields = append(logFields, name+"_cpu", res.CPUProfileKey)
			}
			if res.AllocBytesProfileKey != "" {
				logFields = append(logFields, name+"_alloc_bytes", res.AllocBytesProfileKey)
			}
			if res.AllocCountProfileKey != "" {
				logFields = append(logFields, name+"_alloc_objects", res.AllocCountProfileKey)
			}
		}
		if r.base != nil {
			res, err := r.result.base.runBenchmark(ctx, args.BenchTime, r.key.benchmark)
			if err != nil {
				level.Error(b.logger).Log("msg", "error running benchmark", "package", r.base.meta.ImportPath, "benchmark", r.key.benchmark, "err", err)
			}
			handleResult("base", res)
		}
		if r.head != nil {
			res, err := r.result.head.runBenchmark(ctx, args.BenchTime, r.key.benchmark)
			if err != nil {
				level.Error(b.logger).Log("msg", "error running benchmark", "package", r.base.meta.ImportPath, "benchmark", r.key.benchmark, "err", err)
			}
			handleResult("head", res)
		}
	}

	return nil
}

type resultKey struct {
	packagePath string
	benchmark   string
}

type resultWithKey struct {
	*result
	key resultKey
}

type result struct {
	base   *Package
	head   *Package
	reason string
}

type benchReason uint8

const (
	benchReasonUnkown benchReason = iota
	benchReasonCodeChange
	benchReasonBaseMissing
	benchReasonHeadMissing
)

type resultMaps struct {
	m       map[resultKey]int
	results []result
}

func newResultMaps(len int) *resultMaps {
	return &resultMaps{
		m:       make(map[resultKey]int, len),
		results: make([]result, 0, len),
	}
}

func (r *resultMaps) get(k resultKey) *result {
	idx, ok := r.m[k]
	if !ok {
		idx = len(r.results)
		r.m[k] = idx
		r.results = append(r.results, result{})
	}
	return &r.results[idx]
}

func (b *Benchmark) compareResult() []resultWithKey {
	r := newResultMaps(len(b.headPackages))

	resultFromPackages(func(k resultKey, p *Package) {
		x := r.get(k)
		x.head = p
	}, b.headPackages)
	resultFromPackages(func(k resultKey, p *Package) {
		x := r.get(k)
		x.base = p
	}, b.basePackages)

	if len(r.results) == 0 {
		return nil
	}

	keys := make([]resultKey, len(r.m))
	for k, v := range r.m {
		keys[v] = k
	}

	benchmarkToBeRun := make([]resultWithKey, 0, len(r.results))
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
			resultWithKey{
				key:    k,
				result: res,
			},
		)
		level.Debug(b.logger).Log("msg", "benchmark will be run", "package", res.head.meta.ImportPath, "benchmark", k.benchmark, "reason", res.reason)
	}

	return benchmarkToBeRun
}

func resultFromPackages(f func(resultKey, *Package), pkgs []Package) {
	for idx := range pkgs {
		p := &pkgs[idx]
		for _, b := range p.benchmarkNames {
			f(resultKey{p.meta.ImportPath, b}, p)
		}
	}
}
