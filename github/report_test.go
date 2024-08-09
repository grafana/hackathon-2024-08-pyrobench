package github

import (
	"errors"
	"strings"
	"testing"
	"text/template"

	"github.com/grafana/pyrobench/report"
	"github.com/stretchr/testify/require"
)

func TestGithubCommentTemplate(t *testing.T) {
	tmpl, err := template.New("github").Parse(reportTemplate)
	require.NoError(t, err)

	gh := &gitHubComment{
		template: tmpl,
		githubCommon: githubCommon{
			owner: "my-org",
			repo:  "my-repo",
		},
	}

	for _, tc := range []struct {
		Name     string
		R        *report.BenchmarkReport
		expected string
	}{
		{
			Name: "message",
			R: &report.BenchmarkReport{
				Message: "no benchmarks to be run",
			},
			expected: strings.Join([]string{
				"### Benchmark Report",
				"",
				"__In progress__",
				"",
				"no benchmarks to be run",
				"",
			}, "\n"),
		},
		{
			Name: "error",
			R: &report.BenchmarkReport{
				Error: errors.New("fatally bad"),
			},
			expected: strings.Join([]string{
				"### Benchmark Report",
				"",
				"```",
				"fatally bad",
				"```",
				"",
			}, "\n"),
		},
		{
			Name: "benchmark about to run",
			R: &report.BenchmarkReport{
				BaseRef: "abcd",
				HeadRef: "ef00",
				Runs: []report.BenchmarkRun{
					{
						Name: "pkg1.BenchTestA",
					},
					{
						Name: "pkg1.BenchTestB",
					},
				},
			},
			expected: `### Benchmark Report

__In progress__

abcd -> ef00 ([compare](https://github.com/my-org/my-repo/compare/abcd...ef00))
<details>
    <summary><tt>pkg1.BenchTestA</tt>(scheduled)</summary>

| Resource | Base | Head | Diff % |
|----------|-----:|-----:|-------:|
</details>
<details>
    <summary><tt>pkg1.BenchTestB</tt>(scheduled)</summary>

| Resource | Base | Head | Diff % |
|----------|-----:|-----:|-------:|
</details>
`,
		},
		{
			Name: "benchmark one finished to run",
			R: &report.BenchmarkReport{
				BaseRef: "abcd",
				HeadRef: "ef00",
				Runs: []report.BenchmarkRun{
					{
						Name: "pkg1.BenchTestA",
						Results: []report.BenchmarkResult{
							{
								Name:      "cpu",
								Unit:      "ns",
								BaseValue: report.BenchmarkValue{10000000, "a-cpu-base"},
								HeadValue: report.BenchmarkValue{20000000, "a-cpu-head"},
							},
							{
								Name:      "alloc_space",
								Unit:      "bytes",
								BaseValue: report.BenchmarkValue{2048 * 1024, "a-alloc-base"},
								HeadValue: report.BenchmarkValue{2047 * 1024, "a-alloc-head"},
							},
						},
					},
					{
						Name: "pkg1.BenchTestB",
					},
				},
			},
			expected: `### Benchmark Report

__In progress__

abcd -> ef00 ([compare](https://github.com/my-org/my-repo/compare/abcd...ef00))
<details>
    <summary><tt>pkg1.BenchTestA</tt>(cpu=100 %, alloc_space=-0.04 %)</summary>

| Resource | Base | Head | Diff % |
|----------|-----:|-----:|-------:|
| cpu | [10 ms](https://flamegraph.com/share/a-cpu-base) | [20 ms](https://flamegraph.com/share/a-cpu-head) | [100 %](https://flamegraph.com/share/a-cpu-base/a-cpu-head) |
| alloc_space | [2.0 MiB](https://flamegraph.com/share/a-alloc-base) | [2.0 MiB](https://flamegraph.com/share/a-alloc-head) | [-0.04 %](https://flamegraph.com/share/a-alloc-base/a-alloc-head) |
</details>
<details>
    <summary><tt>pkg1.BenchTestB</tt>(scheduled)</summary>

| Resource | Base | Head | Diff % |
|----------|-----:|-----:|-------:|
</details>
`,
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			body, err := gh.render(tc.R)
			require.NoError(t, err)
			require.Equal(t, tc.expected, body)
		})
	}
}
