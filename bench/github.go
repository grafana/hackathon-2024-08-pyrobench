package bench

import (
	"context"
	"fmt"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log/level"

	"github.com/grafana/pyrobench/github"
	"github.com/grafana/pyrobench/report"
)

func AddGitHubCommentHookCommand(app *kingpin.Application) (*kingpin.CmdClause, *github.CommentHookArgs) {
	cmd := app.Command("github-comment-hook", "Use this in a Github comment workflow to add benchmarks to your repo.")
	return cmd, github.AddCommentHookArgs(cmd)
}

func (b *Benchmark) GitHubCommentHook(ctx context.Context, args *github.CommentHookArgs) error {
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

	gch, err := github.NewCommentHook(ctx, b.logger, args)
	if err != nil {
		return err
	}

	updateCh := make(chan *report.BenchmarkReport)
	reporter, err := gch.Reporter(updateCh)
	if err != nil {
		return err
	}
	defer reporter.Stop()

	r, err := gch.ParseBenchmarks(ctx)
	if err != nil {
		updateCh <- b.generateReport(nil).WithError(err)
		return err
	}
	if len(r.Filter) == 0 {
		// nothing to do, pyrobench has most likely not been mentioned
		level.Info(b.logger).Log("msg", "no command supplied, nothing to do")
		return nil
	}

	// ensure the codebase is checked out
	if _, err := git("init", "."); err != nil {
		return fmt.Errorf("error git init: %w", err)
	}

	remote := "origin"
	if _, err := git("remote", "add", remote, r.GitURL); err != nil {
		return fmt.Errorf("error git remote add: %w", err)
	}

	if _, err := git("fetch", "--depth", "1", remote, r.Base); err != nil {
		return fmt.Errorf("error fetching base: %w", err)
	}

	if _, err := git("fetch", "--depth", "1", remote, r.Head+":head"); err != nil {
		return fmt.Errorf("error fetching head: %w", err)
	}

	if _, err := git("checkout", "head"); err != nil {
		return fmt.Errorf("error checking out head: %w", err)
	}

	filters := make([]*BenchmarkFilter, 0, len(r.Filter))
	for _, f := range r.Filter {
		filters = append(filters, &BenchmarkFilter{
			Filter: f.Regex.Regexp,
			Time:   f.Time,
			Count:  f.Count,
		})
	}

	return b.compareWithReporter(ctx, &CompareArgs{
		BenchTime:  "2s",
		BenchCount: 5,
		Report:     args.Reporter,
		GitBase:    remote + "/" + r.Base,
	}, updateCh, filters...)

}
