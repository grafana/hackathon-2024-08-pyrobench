package report

import (
	"fmt"
	"html/template"
	"strings"
	"sync"

	"github.com/alecthomas/kingpin/v2"
	"github.com/dustin/go-humanize"
	"github.com/go-kit/log"
	"github.com/google/go-github/v63/github"
	"github.com/grafana/pyrobench/benchtab"
)

const baseURL = "https://flamegraph.com"

type BenchmarkReport struct {
	BaseRef string
	HeadRef string
	Runs    []BenchmarkRun
}

type BenchmarkRun struct {
	Name            string
	Results         []BenchmarkResult
	BenchStatTables *benchtab.Tables
}

type BenchmarkValue struct {
	ProfileValue  int64
	FlamegraphKey string
}

func (v *BenchmarkValue) markdown(unit string) string {
	if v.FlamegraphKey == "" {
		return "n/a"
	}

	var val string
	var suffix string
	switch unit {
	case "ns":
		val = humanize.SI(float64(v.ProfileValue)/1e9, "s")
		suffix = "sec/op"
	case "bytes":
		val = humanize.IBytes(uint64(v.ProfileValue))
		suffix = "B/op"
	case "":
		val = humanize.SI(float64(v.ProfileValue), "")
		suffix = "allocs/op"
	}
	val = strings.TrimSpace(val)

	return fmt.Sprintf(
		"[%s](%s/share/%s) (%s)",
		val,
		baseURL,
		v.FlamegraphKey,
		suffix,
	)
}

// this is for cpu, mem, etc
type BenchmarkResult struct {
	Name                 string
	Unit                 string
	BaseValue, HeadValue BenchmarkValue
}

func (r *BenchmarkResult) BaseMarkdown() string {
	return r.BaseValue.markdown(r.Unit)
}

func (r *BenchmarkResult) HeadMarkdown() string {
	return r.HeadValue.markdown(r.Unit)
}

func (r *BenchmarkResult) DiffMarkdown() string {
	if r.BaseValue.FlamegraphKey == "" || r.HeadValue.FlamegraphKey == "" {
		return "n/a"
	}

	diff := float64(r.HeadValue.ProfileValue-r.BaseValue.ProfileValue) / float64(r.BaseValue.ProfileValue) * 100

	return fmt.Sprintf(
		"[%s %%](%s/share/%s/%s)",
		humanize.CommafWithDigits(diff, 2),
		baseURL,
		r.BaseValue.FlamegraphKey,
		r.HeadValue.FlamegraphKey,
	)
}

type gitHubComment struct {
	logger log.Logger
	params *Args

	owner, repo string
	issueNumber int
	commentID   int64 // this is a unique identifier for the comment
	client      *github.Client
	template    *template.Template
	finished    bool

	ch     <-chan *BenchmarkReport
	stopCh chan struct{}
	wg     sync.WaitGroup
}

type Args struct {
	GitHubCommenter     bool
	ConsoleCommenter    bool
	PercentageThreshold float64 // percentage of difference between the base and the value that will trigger a warning
}

func AddArgs(cmd *kingpin.CmdClause) *Args {
	args := &Args{}
	cmd.Flag("github-commenter", "Enable reporting with github commenter").Default("false").BoolVar(&args.GitHubCommenter)
	cmd.Flag("console-commenter", "Enable reporting with console commenter").Default("false").BoolVar(&args.ConsoleCommenter)
	cmd.Flag("percentage-threshold", "Percentage of difference between the base and the value that will trigger a warning").Default("5").Float64Var(&args.PercentageThreshold)
	return args
}

type Reporter interface {
	Stop() error
}

func NewConsoleReporter(ch <-chan *BenchmarkReport) Reporter {
	r := &consoleReporter{
		ch:     ch,
		stopCh: make(chan struct{}),
	}
	r.wg.Add(1)
	go r.run()
	return r
}

type consoleReporter struct {
	ch     <-chan *BenchmarkReport
	stopCh chan struct{}
	wg     sync.WaitGroup
}

func (r *consoleReporter) Stop() error {
	close(r.stopCh)
	r.wg.Wait()
	return nil
}

func (r *consoleReporter) run() {
	defer r.wg.Done()

	for {
		select {
		case <-r.stopCh:
			return
		case report := <-r.ch:
			if report == nil {
				continue
			}

			if len(report.Runs) == 0 {
				continue
			}

			fmt.Printf("Benchmark Report\n")
			fmt.Printf("Base: %s\n", report.BaseRef)
			fmt.Printf("Head: %s\n", report.HeadRef)

			for _, run := range report.Runs {
				fmt.Printf("\nBenchmark: %s\n", run.Name)
				fmt.Println("Markdown")
				for _, result := range run.Results {
					fmt.Printf("  %s\n", result.BaseMarkdown())
					fmt.Printf("  %s\n", result.HeadMarkdown())
				}

				if run.BenchStatTables == nil {
					continue
				}

				tables := run.BenchStatTables.Tables
				for _, table := range tables {
					printTable(table)
				}
			}
		}
	}
}

func printTable(table *benchtab.Table) {
	fmt.Println("cols", table.Cols)
	fmt.Println("rows", table.Rows)
	fmt.Println("unit", table.Unit)

	fmt.Println("summary")
	for _, col := range table.Cols {
		fmt.Println("  col", col.String(), "summary", table.Summary[col].Summary)
	}
}

type noopReporter struct {
}

func (r *noopReporter) Stop() error {
	return nil
}

func NewNoop(ch <-chan *BenchmarkReport) Reporter {
	if ch != nil {
		go func() {
			for range ch {
			}
		}()
	}
	return &noopReporter{}
}
