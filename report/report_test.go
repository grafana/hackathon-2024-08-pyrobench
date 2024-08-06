package report

import (
	"html/template"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGithubCommentTemplate(t *testing.T) {
	tmpl, err := template.New("github").Parse(githubTemplate)
	require.NoError(t, err)

	gh := &gitHubComment{
		template: tmpl,
	}

	for _, tc := range []struct {
		Name     string
		R        *BenchmarkReport
		expected string
	}{
		{
			Name: "benchmark about to run",
			R: &BenchmarkReport{
				BaseRef: "abcd",
				HeadRef: "ef00",
				Runs: []BenchmarkRun{
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

abcd -> ef00
<details>
<summary><tt>pkg1.BenchTestA</tt></summary>

| Resource | Base | Head | Diff % |
|----------|-----:|-----:|-------:|
</details>
<details>
<summary><tt>pkg1.BenchTestB</tt></summary>

| Resource | Base | Head | Diff % |
|----------|-----:|-----:|-------:|
</details>
`,
		},
		{
			Name: "benchmark one finished to run",
			R: &BenchmarkReport{
				BaseRef: "abcd",
				HeadRef: "ef00",
				Runs: []BenchmarkRun{
					{
						Name: "pkg1.BenchTestA",
						Results: []BenchmarkResult{
							{
								Name:      "cpu",
								Unit:      "ns",
								BaseValue: BenchmarkValue{100, "a-cpu-base"},
								HeadValue: BenchmarkValue{200, "a-cpu-head"},
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

abcd -> ef00
<details>
<summary><tt>pkg1.BenchTestA</tt></summary>

| Resource | Base | Head | Diff % |
|----------|-----:|-----:|-------:|
| cpu | [100](https://flamegraph.com/share/a-cpu-base) | [200](https://flamegraph.com/share/a-cpu-head) | [100.00%](https://flamegraph.com/share/a-cpu-base/a-cpu-head) |
</details>
<details>
<summary><tt>pkg1.BenchTestB</tt></summary>

| Resource | Base | Head | Diff % |
|----------|-----:|-----:|-------:|
</details>
`,
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			require.Equal(t, tc.expected, gh.render(tc.R))
			gh.render(tc.R)
		})
	}
}
