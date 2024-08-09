package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log/level"
	"golang.org/x/sync/errgroup"

	"github.com/grafana/pyrobench/github"
	"github.com/grafana/pyrobench/report"
)

type CompareArgs struct {
	GitBase    string
	BenchTime  string
	BenchCount uint16
	Report     *report.Args
	GitHub     *github.Args
}

func AddCompareCommand(app *kingpin.Application) (*kingpin.CmdClause, *CompareArgs) {
	cmd := app.Command("compare", "Compare Golang Mirco Benchmarks using CPU/Memory profiles.")
	args := CompareArgs{
		Report: report.AddArgs(cmd),
		GitHub: github.AddArgs(cmd),
	}
	cmd.Flag("git-base", "Git base commit").Default("HEAD~1").StringVar(&args.GitBase)
	cmd.Flag("bench-time", "Golang's benchtime argument.").Default("2s").StringVar(&args.BenchTime)
	cmd.Flag("bench-count", "Golang's count argument. How often to repeat the benchmarks").Default("6").Uint16Var(&args.BenchCount)
	return cmd, &args
}

type BenchmarkFilter struct {
	Filter *regexp.Regexp
	Time   *string
	Count  *int
}

func (b *Benchmark) Compare(ctx context.Context, args *CompareArgs, filter ...*BenchmarkFilter) error {
	updateCh := make(chan *report.BenchmarkReport)
	if args.Report != nil && args.Report.GitHubCommenter {
		reporter, err := github.NewCommentReporter(b.logger, args.GitHub, updateCh)
		if err != nil {
			return fmt.Errorf("error initializing github reporter: %w", err)
		}
		defer reporter.Stop()
	} else if args.Report != nil && args.Report.ConsoleCommenter {
		reporter := report.NewConsoleReporter(updateCh)
		defer reporter.Stop()
	} else {
		defer report.NewNoop(updateCh).Stop()
	}

	return b.compareWithReporter(ctx, args, updateCh, filter...)
}

func (b *Benchmark) compareWithReporter(ctx context.Context, args *CompareArgs, updateCh chan *report.BenchmarkReport, filter ...*BenchmarkFilter) error {
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

	// listing benchmarks
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for _, pkgs := range [][]Package{b.basePackages, b.headPackages} {
		for idx := range pkgs {
			p := &pkgs[idx]
			g.Go(func() error {

				// list benchmarks first
				if err := p.listBenchmarksAst(gctx, filter); err != nil {
					return err
				}

				// exit early when no benchmarks
				if len(p.benchmarkNames) == 0 {
					return nil
				}

				return p.compileTest(gctx)
			})
		}
	}
	err = g.Wait()
	if err != nil {
		return err
	}
	benchmarks := b.compareResult()
	if len(benchmarks) == 0 {
		msg := "no benchmarks to run"
		updateCh <- b.generateReport(nil).WithMessage(msg)
		level.Info(b.logger).Log("msg", msg)
		return nil
	}
	updateCh <- b.generateReport([][]*benchWithKey{benchmarks})

	level.Info(b.logger).Log("msg", "compiling packages with tests to figure out what changed", "base", countPackagesWithTests(basePackages), "head", countPackagesWithTests(headPackages))
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(4)
	for _, pkgs := range [][]Package{b.basePackages, b.headPackages} {
		for idx := range pkgs {
			p := &pkgs[idx]
			g.Go(func() error {
				// exit early when no benchmarks
				if len(p.benchmarkNames) == 0 {
					return nil
				}

				return p.compileTest(gctx)
			})
		}
	}
	err = g.Wait()
	if err != nil {
		return err
	}

	benchmarks = b.compareResult()
	if len(benchmarks) == 0 {
		msg := "no benchmarks to run"
		updateCh <- b.generateReport(nil).WithMessage(msg)
		level.Info(b.logger).Log("msg", msg)
		return nil
	}

	var benchmarkGroups [][]*benchWithKey

	if len(filter) == 0 {
		filter = append(filter, &BenchmarkFilter{})
		benchmarkGroups = append(benchmarkGroups, benchmarks)
	} else {
		var somethingMatched bool
		benchmarkGroups = make([][]*benchWithKey, len(filter))
		for idx, f := range filter {
			for _, b := range benchmarks {
				if f.Filter.MatchString(b.key.benchmark) {
					newB := *b
					somethingMatched = true

					benchmarkGroups[idx] = append(benchmarkGroups[idx], &newB)
				}
			}
		}
		if !somethingMatched {
			msg := "no benchmarks to run"
			updateCh <- b.generateReport(nil).WithMessage(msg)
			level.Info(b.logger).Log("msg", msg)
			return nil
		}
	}

	updateCh <- b.generateReport(benchmarkGroups)
	for idx, benchmarks := range benchmarkGroups {
		for _, r := range benchmarks {
			f := filter[idx]

			benchTime := args.BenchTime
			if f.Time != nil {
				benchTime = *f.Time
			}
			benchCount := args.BenchCount
			if f.Count != nil {
				benchCount = uint16(*f.Count)
			}

			if r.base != nil {
				res, err := r.bench.base.runBenchmark(ctx, benchTime, benchCount, r.key.benchmark)
				if err != nil {
					level.Error(b.logger).Log("msg", "error running benchmark", "package", r.base.meta.ImportPath, "benchmark", r.key.benchmark, "err", err)
				}

				b.addBenchStatResults(res, benchSourceBase)
				r.addResult(benchSourceBase, res)
				// updateCh <- b.generateReport(benchmarkGroups, nil)
			}
			if r.head != nil {
				res, err := r.bench.head.runBenchmark(ctx, benchTime, benchCount, r.key.benchmark)
				if err != nil {
					level.Error(b.logger).Log("msg", "error running benchmark", "package", r.base.meta.ImportPath, "benchmark", r.key.benchmark, "err", err)
				}

				b.addBenchStatResults(res, benchSourceHead)
				r.addResult(benchSourceHead, res)
				// updateCh <- b.generateReport(benchmarkGroups, nil)
			}

			tables := b.statBuilders[r.key.benchmark].ToTables()
			tables.ToText(os.Stdout, false)
			r.tables = tables

			updateCh <- b.generateReport(benchmarkGroups)

		}

	}

	close(updateCh)
	return nil
}
