package github

import (
	"context"
	"strings"
	"text/template"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/google/go-github/v63/github"
	"github.com/grafana/pyrobench/report"
)

type CommentReporterArgs struct {
	GitHub *Args
	Report *report.Args
}

func NewCommentReporter(logger log.Logger, args *Args, ch <-chan *report.BenchmarkReport) (report.Reporter, error) {
	ghCommon, _, err := newGitHubCommon(args)
	if err != nil {
		return nil, err
	}

	return newCommentReporterFromGitHubCommon(logger, ghCommon, ch)
}

func newCommentReporterFromGitHubCommon(logger log.Logger, ghCommon *githubCommon, ch <-chan *report.BenchmarkReport) (report.Reporter, error) {
	tmpl, err := template.New("github").Parse(reportTemplate)
	if err != nil {
		return nil, err
	}

	gh := &gitHubComment{
		logger:       log.With(logger, "module", "github-commenter"),
		ch:           ch,
		githubCommon: *ghCommon,
		stopCh:       make(chan struct{}),
		template:     tmpl,
	}

	gh.wg.Add(1)
	go func() {
		defer gh.wg.Done()
		gh.run(context.Background())
	}()

	return gh, nil
}
func (gh *gitHubComment) react(ctx context.Context, content string) error {
	if gh.eventCommentID == 0 {
		return nil
	}
	_, _, err := gh.githubCommon.client.Reactions.CreateIssueCommentReaction(
		ctx,
		gh.owner,
		gh.repo,
		gh.eventCommentID,
		content,
	)
	return err
}

func (gh *gitHubComment) render(re *report.BenchmarkReport) (string, error) {
	buf := &strings.Builder{}
	if err := gh.template.Execute(buf, struct {
		Report  *report.BenchmarkReport
		Compare string
	}{
		Report:  re,
		Compare: re.MarkdownCompare(gh.owner, gh.repo),
	}); err != nil {
		return "", err
	}
	return buf.String(), nil
}
func (gh *gitHubComment) postReport(ctx context.Context, report *report.BenchmarkReport) error {
	body, err := gh.render(report)
	if err != nil {
		return err
	}
	if report.Error != nil {
		if err := gh.react(ctx, "confused"); err != nil {
			level.Warn(gh.logger).Log("msg", "failed to add reaction to issue comment", "err", err)
		}
	} else if !gh.reacted {
		if err := gh.react(ctx, "eyes"); err != nil {
			level.Warn(gh.logger).Log("msg", "failed to add reaction to issue comment", "err", err)
		}
		gh.reacted = true
	}

	return gh.postComment(ctx, body)
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
		gh.githubCommon.owner,
		gh.githubCommon.repo,
		gh.githubCommon.pr,
		&github.IssueComment{
			Body: &body,
		},
	)
	gh.commentID = resp.GetID()
	return err

}

func (gh *gitHubComment) run(ctx context.Context) {
	var lastReport *report.BenchmarkReport
	defer func() {
		// finish the report if it's not finished
		if lastReport != nil && !lastReport.Finished {
			lastReport.Finished = true
			if err := gh.postReport(ctx, lastReport); err != nil {
				level.Warn(gh.logger).Log("msg", "failed to post report", "err", err)
			}
		}
	}()
	for {
		select {
		case <-gh.stopCh:
			// finalize something
			return
		case report, ok := <-gh.ch:
			if !ok {
				return
			}
			if err := gh.postReport(ctx, report); err != nil {
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
