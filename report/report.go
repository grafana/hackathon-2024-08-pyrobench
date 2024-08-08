package report

import (
	"fmt"
	"html/template"
	"sync"

	"github.com/alecthomas/kingpin/v2"
	"github.com/dustin/go-humanize"
	"github.com/go-kit/log"
	"github.com/google/go-github/v63/github"
)

const baseURL = "https://flamegraph.com"

type BenchmarkReport struct {
	BaseRef string
	HeadRef string
	Runs    []BenchmarkRun
}

type BenchmarkRun struct {
	Name    string
	Results []BenchmarkResult
}

type BenchmarkValue struct {
	ProfileValue  int64
	FlamegraphKey string
}

func (v *BenchmarkValue) markdown(unit string) string {
	if v.FlamegraphKey == "" {
		return "n/a"
	}
	n := humanize.SI(float64(v.ProfileValue), "")
	if unit == "bytes" {
		n = humanize.IBytes(uint64(v.ProfileValue))
	}
	if unit == "ns" {
		n = humanize.SI(float64(v.ProfileValue)/1e9, "s")
	}
	return fmt.Sprintf(
		"[%s](%s/share/%s)",
		n,
		baseURL,
		v.FlamegraphKey,
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
	PercentageThreshold float64 // percentage of difference between the base and the value that will trigger a warning
}

func AddArgs(cmd *kingpin.CmdClause) *Args {
	args := &Args{}
	cmd.Flag("github-commenter", "Enable reporting with github commenter").Default("false").BoolVar(&args.GitHubCommenter)
	cmd.Flag("percentage-threshold", "Percentage of difference between the base and the value that will trigger a warning").Default("5").Float64Var(&args.PercentageThreshold)
	return args
}

type Reporter interface {
	Stop() error
}

type noopReporter struct{}

func (_ noopReporter) Stop() error { return nil }

func NewNoop(ch <-chan *BenchmarkReport) Reporter {
	if ch != nil {
		go func() {
			for range ch {
			}
		}()
	}
	return &noopReporter{}
}
