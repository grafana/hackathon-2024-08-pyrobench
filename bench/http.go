package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
)

type profileResponse struct {
	URL         string `json:"url"`
	Key         string `json:"key"`
	SubProfiles []struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"subProfiles"`
}

func uploadProfile(ctx context.Context, logger log.Logger, body io.Reader) (*profileResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://flamegraph.com", body)
	if err != nil {
		return nil, err
	}

	// TODO: Fill refere to point to github PR
	req.Header.Set("user-agent", "pyrobench")
	req.Header.Set("content-type", "application/octet-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	x, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to upload profile: [%d] msg=%s", resp.StatusCode, string(x))
	}

	result := profileResponse{}
	err = json.Unmarshal(x, &result)
	if err != nil {
		return nil, err
	}

	level.Debug(logger).Log("msg", "uploaded profile", "url", result.URL)

	return &result, nil

}
