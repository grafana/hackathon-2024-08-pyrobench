package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"github.com/grafana/pyrobench/bench"
)

var (
	consoleOutput = os.Stderr
	logger        = log.NewLogfmtLogger(consoleOutput)
)

var cfg struct {
	verbose bool
}

func checkError(err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func main() {
	ctx := context.Background()
	b, err := bench.New(logger)
	if err != nil {
		os.Exit(checkError(err))
	}

	app := kingpin.New(filepath.Base(os.Args[0]), "Compare Golang Mirco Benchmarks using CPU/Memory profiles.").UsageWriter(consoleOutput)
	app.Flag("verbose", "Enable verbose logging.").Short('v').Default("0").BoolVar(&cfg.verbose)

	compareCmd, compareArgs := bench.AddCompareCommand(app)

	gitHubCommentHookCmd, githubCommentHookArgs := bench.AddGitHubCommentHookCommand(app)

	// parse command line arguments
	parsedCmd := kingpin.MustParse(app.Parse(os.Args[1:]))

	// enable verbose logging if requested
	if !cfg.verbose {
		logger = level.NewFilter(logger, level.AllowInfo())
	}

	switch parsedCmd {
	case compareCmd.FullCommand():
		if err := b.Compare(ctx, compareArgs); err != nil {
			os.Exit(checkError(err))
		}
	case gitHubCommentHookCmd.FullCommand():
		if err := b.GitHubCommentHook(ctx, githubCommentHookArgs); err != nil {
			os.Exit(checkError(err))
		}
	default:
		_ = level.Error(logger).Log("msg", "unknown command", "cmd", parsedCmd)
	}

}
