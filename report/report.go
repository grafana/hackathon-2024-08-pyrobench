package report

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/alecthomas/kingpin/v2"
	"github.com/dustin/go-humanize"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
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
	n := humanize.Comma(v.ProfileValue)
	if unit == "bytes" {
		n = humanize.Bytes(uint64(v.ProfileValue))
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
	params *Params

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

type Params struct {
	GitHubCommenter     bool
	PercentageThreshold float64 // percentage of difference between the base and the value that will trigger a warning
}

func AddParams(cmd *kingpin.CmdClause) *Params {
	args := &Params{}
	cmd.Flag("github-commenter", "Enable reporting with github commenter").Default("false").BoolVar(&args.GitHubCommenter)
	cmd.Flag("percentage-threshold", "Percentage of difference between the base and the value that will trigger a warning").Default("5").Float64Var(&args.PercentageThreshold)

	return args
}

type Reporter interface {
	Stop() error
}

type noopReporter struct{}

func (_ noopReporter) Stop() error { return nil }

func newNoopReporter(ch <-chan *BenchmarkReport) Reporter {
	if ch != nil {
		go func() {
			for range ch {
			}
		}()
	}
	return &noopReporter{}
}

func New(logger log.Logger, params *Params, ch <-chan *BenchmarkReport) (Reporter, error) {
	if params != nil && params.GitHubCommenter {
		return newGithHubComment(logger, params, ch)
	}

	return newNoopReporter(ch), nil
}

func parseGitHubRef(ref string) (issueNumber int, ok bool) {
	prefix := "refs/pull/"
	if !strings.HasPrefix(ref, prefix) {
		return 0, false
	}
	suffix := "/merge"
	if !strings.HasSuffix(ref, "/merge") {
		return 0, false
	}

	issueNumber, err := strconv.Atoi(ref[len(prefix) : len(ref)-len(suffix)])
	if err != nil {
		return 0, false
	}
	return issueNumber, true
}

func newGithHubComment(logger log.Logger, params *Params, ch <-chan *BenchmarkReport) (Reporter, error) {
	githubToken := os.Getenv("GITHUB_TOKEN")
	if githubToken == "" {
		return nil, errors.New("GITHUB_TOKEN is required for github comment reporter")
	}

	githubRepository := os.Getenv("GITHUB_REPOSITORY")
	if githubRepository == "" {
		return nil, errors.New("GITHUB_REPOSITORY is required for github comment reporter")
	}
	parts := strings.SplitN(githubRepository, "/", 2)
	if len(parts) != 2 {
		return nil, errors.New("GITHUB_REPOSITORY must be in the format owner/repo")
	}

	githubRef := os.Getenv("GITHUB_REF")
	if githubRef == "" {
		return nil, errors.New("GITHUB_REF is required for github comment reporter")
	}
	githubIssue, ok := parseGitHubRef(githubRef)
	if !ok {
		level.Warn(logger).Log("msg", "GITHUB_REF must be a pull request ref for a github comment to be active", "ref", githubRef)
		return newNoopReporter(ch), nil
	}

	tmpl, err := template.New("github").Parse(githubTemplate)
	if err != nil {
		return nil, err
	}

	gh := &gitHubComment{
		logger:      log.With(logger, "module", "github-commenter"),
		ch:          ch,
		owner:       parts[0],
		repo:        parts[1],
		issueNumber: githubIssue,
		stopCh:      make(chan struct{}),
		client:      github.NewClient(nil).WithAuthToken(githubToken),
		params:      params,
		template:    tmpl,
	}

	gh.wg.Add(1)
	go func() {
		defer gh.wg.Done()
		gh.run(context.Background())
	}()

	return gh, nil
}

var githubTemplate = `{{- $global := . -}}
### Benchmark Report

{{ if .Finished }}__Finished__{{ else }}__In progress__{{ end }}

{{.Report.BaseRef}} -> {{.Report.HeadRef}}

{{- range .Report.Runs }}
<details>
<summary><tt>{{.Name}}</tt></summary>

| Resource | Base | Head | Diff % |
|----------|-----:|-----:|-------:|
{{- range .Results }}
| {{.Name}} | {{.BaseMarkdown}} | {{.HeadMarkdown}} | {{.DiffMarkdown}} |
{{- end }}
</details>
{{- end }}
`

func (gh *gitHubComment) render(report *BenchmarkReport) string {
	buf := &strings.Builder{}
	if err := gh.template.Execute(buf, struct {
		Report   *BenchmarkReport
		Finished bool
	}{
		Report:   report,
		Finished: gh.finished,
	}); err != nil {
		level.Warn(gh.logger).Log("msg", "failed to render template", "err", err)
	}
	return buf.String()
}

func (gh *gitHubComment) postComment(ctx context.Context, body string) error {
	if gh.commentID != 0 {
		// update an existing comment
		_, _, err := gh.client.Issues.EditComment(
			ctx,
			gh.owner,
			gh.repo,
			gh.commentID,
			&github.IssueComment{
				Body: &body,
			},
		)
		return err
	}

	resp, _, err := gh.client.Issues.CreateComment(
		ctx,
		gh.owner,
		gh.repo,
		gh.issueNumber,
		&github.IssueComment{
			Body: &body,
		},
	)
	gh.commentID = resp.GetID()
	return err

}

func (gh *gitHubComment) run(ctx context.Context) {
	var lastReport *BenchmarkReport
	defer func() {
		gh.finished = true
		body := gh.render(lastReport)
		if err := gh.postComment(ctx, body); err != nil {
			level.Warn(gh.logger).Log("msg", "failed to post comment", "err", err)
		}
	}()
	for {
		select {
		case <-gh.stopCh:
			// finalize something
			return
		case report := <-gh.ch:
			body := gh.render(report)
			if err := gh.postComment(ctx, body); err != nil {
				level.Warn(gh.logger).Log("msg", "failed to post comment", "err", err)
			}
			lastReport = report
		}
	}
}

func (gh *gitHubComment) Stop() error {
	close(gh.stopCh)
	gh.wg.Wait()
	return nil
}

func main() {
	fmt.Println("vim-go")
}
