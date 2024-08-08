package github

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseCommandLine(t *testing.T) {
	args := &CommentHookArgs{
		BotName: "@pyrobench",
	}

	for _, tc := range []struct {
		name        string
		line        string
		expectedErr string
		result      string
	}{
		{
			name:   "run single benchmark",
			line:   "@pyrobench E2E\n",
			result: `[{"regex":"E2E"}]`,
		},
		{
			name:   "run single benchmark whitespace",
			line:   "@pyrobench E2E ",
			result: `[{"regex":"E2E"}]`,
		},
		{
			name:   "run single benchmark newline",
			line:   "@pyrobench E2E\n",
			result: `[{"regex":"E2E"}]`,
		},
		{
			name:   "run single benchmark with custom count",
			line:   "@pyrobench E2E count=10",
			result: `[{"regex":"E2E", "count":10}]`,
		},
		{
			name:   "run single benchmark with custom time",
			line:   "@pyrobench E2E time=5x",
			result: `[{"regex":"E2E", "time":"5x"}]`,
		},
		{
			name:        "regex error",
			line:        "@pyrobench E2[E",
			expectedErr: "failed to compile regex: error parsing regexp: missing closing ]",
		},
		{
			name:   "run two benchmarks on two lines",
			line:   "@pyrobench E2E\n@pyrobench E4E",
			result: `[{"regex":"E2E"},{"regex":"E4E"}]`,
		},
		{
			name:        "option without benchmark",
			line:        "@pyrobench count=1",
			expectedErr: "option 'count=1 'given before benchmark",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			act, err := parseCommandLine(args, strings.NewReader(tc.line))

			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				require.Nil(t, act)
				return

			} else {
				require.NoError(t, err)
			}
			actJson, err := json.Marshal(&act)
			require.NoError(t, err)

			if tc.result == "" {
				tc.result = "[]"
			}
			require.JSONEq(t, tc.result, string(actJson))
		})
	}
}
