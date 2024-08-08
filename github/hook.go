package github

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/pyrobench/report"
)

type CommentHookArgs struct {
	*Args
	Reporter            *report.Args
	AllowedAssociations []string
	BotName             string
}

func AddCommentHookArgs(cmd *kingpin.CmdClause) *CommentHookArgs {
	args := &CommentHookArgs{
		Args:     AddRequiredArgs(cmd),
		Reporter: report.AddArgs(cmd),
	}
	cmd.Flag("allowed-associations", "Allowed associations for the comment hook.").Default("collaborator", "contributor", "member", "owner").StringsVar(&args.AllowedAssociations)
	cmd.Flag("bot-name", "What is my name?").Default("@pyrobench").StringVar(&args.BotName)
	return args
}

type CommentHook struct {
	githubCommon
	body string

	logger log.Logger
	args   *CommentHookArgs
}

func NewCommentHook(ctx context.Context, logger log.Logger, args *CommentHookArgs) (*CommentHook, error) {
	ghCommon, ghContext, err := newGitHubCommon(args.Args)
	if err != nil {
		return nil, err
	}

	if exp := "issue_comment"; ghContext.EventName != exp {
		return nil, fmt.Errorf("unsupported event_name in github context: %s, expected %s", ghContext.EventName, exp)
	}

	if ghContext.Event.Action != "created" {
		return nil, fmt.Errorf("unsupported action in github context: %s", ghContext.Event.Action)
	}

	if !slices.Contains(args.AllowedAssociations, strings.ToLower(ghContext.Event.Comment.AuthorAssociation)) {
		return nil, fmt.Errorf("author association %s is not allowed, allowed are %s", ghContext.Event.Comment.AuthorAssociation, strings.Join(args.AllowedAssociations, ", "))
	}

	return &CommentHook{
		logger: logger,
		args:   args,

		body:         ghContext.Event.Comment.Body,
		githubCommon: *ghCommon,
	}, nil

}

type BenchmarkFilter struct {
	Regex *Regexp `json:"regex"`
	Time  *string `json:"time,omitempty"`
	Count *int    `json:"count,omitempty"`
}

func BenchmarkFiltersString(b []*BenchmarkFilter) string {
	sl := make([]string, len(b))
	for i, v := range b {
		sl[i] = v.String()
	}
	return strings.Join(sl, ", ")
}

func (b *BenchmarkFilter) String() string {
	sb := strings.Builder{}
	if b.Regex == nil {
		sb.WriteString("nil")
	} else {
		sb.WriteString(b.Regex.String())
	}
	if b.Time != nil {
		sb.WriteString(fmt.Sprintf(" time=%s", *b.Time))
	}
	if b.Count != nil {
		sb.WriteString(fmt.Sprintf(" count=%d", *b.Count))
	}
	return sb.String()
}

type Regexp struct {
	*regexp.Regexp
}

func (r *Regexp) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.Regexp.String())
}

type CommentHookResult struct {
	Filter []*BenchmarkFilter
	Base   string
	Head   string
	GitURL string
}

func (h *CommentHook) ParseBenchmarks(ctx context.Context) (*CommentHookResult, error) {

	// parse the body to see if we need to get active
	benchmarks, err := parseCommandLine(h.args, strings.NewReader(h.body))
	if err != nil {
		return nil, fmt.Errorf("failed to parse command line: %w", err)
	}
	if len(benchmarks) == 0 {
		// nothing to do
		return &CommentHookResult{}, nil
	}

	level.Info(h.logger).Log("msg", "running benchmarks", "repo", h.owner, "repo", h.repo, "pr", h.args, "benchmarks", BenchmarkFiltersString(benchmarks))

	pr, _, err := h.client.PullRequests.Get(ctx, h.owner, h.repo, h.pr)
	if err != nil {
		return nil, fmt.Errorf("failed to get pull request: %w", err)
	}

	level.Info(h.logger).Log("msg", "read PRs diff from github api", "owner", h.owner, "repo", h.repo, "pr", h.pr, "base", pr.GetBase().GetRef(), "head", pr.GetHead().GetRef())

	return &CommentHookResult{
		Filter: benchmarks,
		Base:   pr.GetBase().GetRef(),
		Head:   fmt.Sprintf("refs/pull/%d/head", h.pr),
		GitURL: pr.GetHead().GetRepo().GetCloneURL(),
	}, nil
}

func (h *CommentHook) Reporter(updateCh <-chan *report.BenchmarkReport) (report.Reporter, error) {
	return newCommentReporterFromGitHubCommon(h.logger, &h.githubCommon, updateCh)
}

func parseCommandLine(args *CommentHookArgs, r io.Reader) ([]*BenchmarkFilter, error) {
	var result []*BenchmarkFilter

	// go through string line by line
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		// find my name
		pos := strings.Index(scanner.Text(), args.BotName)
		if pos < 0 {
			continue
		}

		var current *BenchmarkFilter
		for _, field := range strings.Fields(scanner.Text()[pos+len(args.BotName):]) {
			pos := strings.Index(field, "=")
			if pos < 0 {
				// new regex
				if current != nil {
					result = append(result, current)
				}
				re, err := regexp.Compile(field)
				if err != nil {
					return nil, fmt.Errorf("failed to compile regex: %w", err)
				}

				current = &BenchmarkFilter{Regex: &Regexp{re}}
				continue
			}

			if current == nil {
				return nil, fmt.Errorf("option '%s 'given before benchmark", field)
			}

			if p := "count="; strings.HasPrefix(field, p) {
				count, err := strconv.Atoi(field[len(p):])
				if err != nil {
					return nil, fmt.Errorf("failed to parse count: %w", err)
				}
				current.Count = &count
				continue
			}

			if p := "time="; strings.HasPrefix(field, p) {
				s := strings.Clone(field[len(p):])
				current.Time = &s
				continue
			}

			return nil, fmt.Errorf("unknown option: %s", field)

		}
		if current != nil {
			result = append(result, current)
		}
	}
	switch err := scanner.Err(); err {
	case nil:
		return result, nil
	default:
		return nil, fmt.Errorf("failed to read input: %w", err)
	}
}
