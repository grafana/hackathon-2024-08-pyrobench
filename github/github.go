package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"text/template"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/google/go-github/v63/github"
	"github.com/grafana/pyrobench/report"
)

type Args struct {
	Token   string
	Context string
}

func addArgs(cmd *kingpin.CmdClause, required bool) *Args {
	f := func(f *kingpin.FlagClause) *kingpin.FlagClause {
		if required {
			return f.Required()
		}
		return f
	}
	args := &Args{}
	f(cmd.Flag("github-context", "Github context to use for the comment hook.").Envar("GITHUB_CONTEXT")).StringVar(&args.Context)
	f(cmd.Flag("github-token", "Github token for API use.").Envar("GITHUB_TOKEN")).StringVar(&args.Token)
	return args
}

func AddRequiredArgs(cmd *kingpin.CmdClause) *Args { return addArgs(cmd, true) }

func AddArgs(cmd *kingpin.CmdClause) *Args { return addArgs(cmd, false) }

type githubContext struct {
	Repository string `json:"repository"`
	EventName  string `json:"event_name"`
	Event      struct {
		Action  string `json:"action"`
		Comment struct {
			ID                int64  `json:"id"`
			AuthorAssociation string `json:"author_association"`
			Body              string `json:"body"`
		} `json:"comment"`
		Issue struct {
			Number      int `json:"number"`
			PullRequest struct {
				URL string `json:"url"`
			} `json:"pull_request"`
		} `json:"issue"`
	} `json:"event"`
}

type githubCommon struct {
	body           string
	pr             int
	owner          string
	repo           string
	eventCommentID int64

	client *github.Client
}

func newGitHubCommon(args *Args) (*githubCommon, *githubContext, error) {
	if args.Token == "" {
		return nil, nil, errors.New("GITHUB_TOKEN is required")
	}

	if args.Token == "" {
		return nil, nil, errors.New("GITHUB_CONTEXT is required")
	}

	var ghContext githubContext
	if err := json.Unmarshal([]byte(args.Context), &ghContext); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal github context: %w", err)
	}

	if ghContext.Event.Issue.PullRequest.URL == "" {
		return nil, nil, fmt.Errorf("issue is not a pull request")
	}

	parts := strings.SplitN(ghContext.Repository, "/", 2)
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("invalid repository: %s", ghContext.Repository)
	}

	return &githubCommon{
		pr:             ghContext.Event.Issue.Number,
		owner:          parts[0],
		repo:           parts[1],
		client:         github.NewClient(nil).WithAuthToken(args.Token),
		eventCommentID: ghContext.Event.Comment.ID,
	}, &ghContext, nil
}

type gitHubComment struct {
	githubCommon
	logger log.Logger

	ch       <-chan *report.BenchmarkReport
	stopCh   chan struct{}
	wg       sync.WaitGroup
	template *template.Template

	commentID int64 // this is a unique identifier for the comment
	reacted   bool  // have I reacted to source command yet

	GitHubCommenter bool

	finished bool
}
