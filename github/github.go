package github

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/google/go-github/v63/github"

	"github.com/grafana/pyrobench/report"
)

type CommentHookArgs struct {
	AllowedAssociations []string
	BotName             string
	Report              *report.Args
}

func AddCommentHook(app *kingpin.Application) (*kingpin.CmdClause, *CommentHookArgs) {
	cmd := app.Command("github-comment-hook", "Use this in a Github comment workflow to add benchmarks to your repo.")
	reportParams := report.AddArgs(cmd)
	args := &CommentHookArgs{
		Report: reportParams,
	}
	cmd.Flag("allowed-associations", "Allowed associations for the comment hook.").Default("collaborator", "contributor", "member", "owner").StringsVar(&args.AllowedAssociations)
	cmd.Flag("bot-name", "What is my name?").Default("@pyrobench").StringVar(&args.BotName)
	return cmd, args
}

func CommentHook(ctx context.Context, logger log.Logger, args *CommentHookArgs) error {
	ghContext := os.Getenv("GITHUB_CONTEXT")
	if ghContext == "" {
		return errors.New("GITHUB_CONTEXT is required for github comment hook")
	}
	ghToken := os.Getenv("GITHUB_TOKEN")
	if ghToken == "" {
		return errors.New("GITHUB_TOKEN is required for github comment hook")
	}

	var ghCtx githubContext
	if err := json.Unmarshal([]byte(ghContext), &ghCtx); err != nil {
		return fmt.Errorf("failed to unmarshal GITHUB_CONTEXT: %w", err)
	}

	return commentHook(ctx, logger, args, &ghCtx, ghToken)
}

type githubContext struct {
	Repository string `json:"repository"`
	EventName  string `json:"event_name"`
	Event      struct {
		Action  string `json:"action"`
		Comment struct {
			ID                int64  `json:"id"`
			AuthorAssociation string `json:"author_association"`
		} `json:"comment"`
		Issue struct {
			Number      int `json:"number"`
			PullRequest struct {
				URL string `json:"url"`
			} `json:"pull_request"`
		} `json:"issue"`
	} `json:"event"`
}

type BenchmarkArg struct {
	Regex string
	Time  *string
	Count *int
}

func parseCommandLine(args *CommentHookArgs, r io.Reader) ([]*BenchmarkArg, error) {
	var result []*BenchmarkArg

	// go through string line by line
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		// find my name
		pos := strings.Index(scanner.Text(), args.BotName)
		if pos < 0 {
			continue
		}

		var current *BenchmarkArg
		for _, field := range strings.Fields(scanner.Text()[pos+len(args.BotName):]) {
			pos := strings.Index(field, "=")
			if pos < 0 {
				// new regex
				if current != nil {
					result = append(result, current)
				}
				current = &BenchmarkArg{Regex: field}
			}

			if p := "count="; strings.HasPrefix(field, p) {
				count, err := strconv.Atoi(field[len(p):])
				if err != nil {
					return nil, fmt.Errorf("failed to parse count: %w", err)
				}
				current.Count = &count
			}

			if p := "time="; strings.HasPrefix(field, p) {
				s := strings.Clone(field[len(p):])
				current.Time = &s
			}
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

func commentHook(ctx context.Context, logger log.Logger, args *CommentHookArgs, ghContext *githubContext, ghToken string) error {
	if exp := "issue_comment"; ghContext.EventName != exp {
		return fmt.Errorf("unsupported event_name in github context: %s, expected %s", ghContext.EventName, exp)
	}

	if ghContext.Event.Action != "created" {
		return fmt.Errorf("unsupported action in github context: %s", ghContext.Event.Action)
	}

	if !slices.Contains(args.AllowedAssociations, strings.ToLower(ghContext.Event.Comment.AuthorAssociation)) {
		return fmt.Errorf("author association %s is not allowed, allowed are %s", ghContext.Event.Comment.AuthorAssociation, strings.Join(args.AllowedAssociations, ", "))
	}

	// parse the body to see if we need to get active
	benchmarks, err := parseCommandLine(args, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse command line: %w", err)
	}
	if len(benchmarks) == 0 {
		// nothing to do
		return nil
	}
	level.Info(logger).Log("msg", "running benchmarks", "repo", ghContext.Repository, "pr", ghContext.Event.Issue.Number, "benchmarks", fmt.Sprintf("%+#v", benchmarks))

	gh := github.NewClient(nil).WithAuthToken(ghToken)
	orgRepo := strings.SplitN(ghContext.Repository, "/", 2)
	pr, _, err := gh.PullRequests.Get(ctx, orgRepo[0], orgRepo[1], ghContext.Event.Issue.Number)
	if err != nil {
		return fmt.Errorf("failed to get pull request: %w", err)
	}
	pr.GetBase()

	level.Info(logger).Log("msg", "running benchmarks", "repo", ghContext.Repository, "pr", ghContext.Event.Issue.Number, "base", pr.GetBase().GetRef(), "head", pr.GetHead().GetRef())

	// TODO: Clone the repo and checkout base and head and run benchmark

	return nil

}
