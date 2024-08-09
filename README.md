# Pyrobench

Compare go benchmarks runs with cpu/memory profiles in GitHub actions.

## Usage

In order to use Pyrobench, you need to setup a GitHub action, which reacts to issue comments in your repository.

You can use our GitHub action, by creating a workflow `.github/workflows/pyrobench.yaml` like this:

```yaml
on: issue_comment

jobs:
  pyrobench:
    name: Run Pyrobench on demand by PR comment
    if: ${{ (github.event.issue.pull_request) && contains(github.event.comment.body, '@pyrobench') }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-go@v5
        with:
          go-version: "1.22"
      - name: Pyrobench
        uses: grafana/pyrobench@main
        with:
          github_context: ${{ toJson(github) }}
          github_token: ${{ secrets.GITHUB_TOKEN }}
```

Then within PRs, you can use commands to the bot like this to request benchmark runs:

```
@pyrobench BenchmarkMyHotPath
```

It is also possible to specify multiple bechmark commands and also adapt the options:

```
@pyrobench BenchmarkA count=10 time=10x BenchmarkB count=15 time=2s
```

| Option  | Description                                                                                                     | Default |
| ------- | --------------------------------------------------------------------------------------------------------------- | ------- |
| `count` | How often is a particular benchmark run                                                                         | '6'     |
| `time`  | How long is a single benchmark run, either duration like `10s` or a how often the code gets iterated e.g. '5x'. | '2s'    |
