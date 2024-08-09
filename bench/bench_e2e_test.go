package bench

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/require"

	"github.com/grafana/pyrobench/github"
	"github.com/grafana/pyrobench/report"
)

const (
	// the path to the test repo
	testRepoURL = "https://github.com/simonswine/pyrobench-testdata.git"
	testRepoDir = ".pyrobench-testdata.git"
)

func run(t testing.TB, cmd []string) {
	t.Helper()

	bufErr := bytes.NewBuffer(nil)
	bufOut := bytes.NewBuffer(nil)

	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdout = bufOut
	c.Stderr = bufErr
	err := c.Run()
	if err != nil {
		t.Fatalf("command %v failed: %v\nstdout=%s\nstd=err=%s\n", cmd, err, bufOut.String(), bufErr.String())
	}
}

// TODO: This should locally mock flamegraph.com
func TestBenchmarkE2E(t *testing.T) {
	ctx := context.Background()

	logger := log.NewLogfmtLogger(os.Stderr)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// copy test repo locally, so you can test offline
	if _, err := os.Stat(testRepoDir); os.IsNotExist(err) {
		t.Log("Repo not existing locally, cloning test repo")
		run(t, []string{"git", "clone", "--bare", testRepoURL, testRepoDir})
	} else if err != nil {
		require.NoError(t, err)
	}

	// clone the test repo
	workingDir := t.TempDir()
	run(t, []string{"git", "clone", testRepoDir, workingDir})

	// run the benchmark
	{
		// change to the working directory
		oldCwd, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(workingDir))
		defer func() {
			require.NoError(t, os.Chdir(oldCwd))
		}()

		// run the benchmark
		b, _ := New(logger)
		require.NoError(t, b.Compare(ctx, &CompareArgs{
			GitHub: &github.Args{
				Token:   os.Getenv("GITHUB_TOKEN"),
				Context: os.Getenv("GITHUB_CONTEXT"),
			},
			GitBase:    "HEAD~1",
			BenchTime:  "200ms",
			BenchCount: 6,
			Report: &report.Args{
				GitHubCommenter: os.Getenv("PYROBENCH_GITHUB_REPORT") == "true",
			},
		}))
	}
}
