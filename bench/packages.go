package bench

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
)

type Package struct {
	logger log.Logger

	meta *packageMeta

	testBinary     string
	testBinaryHash []byte
	benchmarkNames []string
}

type packageMeta struct {
	Dir        string
	Root       string
	ImportPath string

	// TestGoFiles is the list of package test source files.
	TestGoFiles []string `json:",omitempty"`
}

func (p *Package) hasNoTests() bool {
	return len(p.meta.TestGoFiles) == 0
}

func (p *Package) listBenchmarks(ctx context.Context) error {
	if p.hasNoTests() {
		return nil
	}

	if p.testBinary == "" {
		return errors.New("test binary not compiled")
	}

	cmd := []string{p.testBinary, "-test.list", "Benchmark.*"}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = p.meta.Dir
	out, err := c.StdoutPipe()
	if err != nil {
		return err
	}

	err = c.Start()
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		p.benchmarkNames = append(p.benchmarkNames, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	return c.Wait()
}

type profileResponse struct {
	URL         string `json:"url"`
	Key         string `json:"key"`
	SubProfiles []struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"suProfiles"`
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

	logger.Log("msg", "uploaded profile", "url", result.URL)

	return &result, nil

}

type benchmarkResult struct {
	ImportPath           string
	Name                 string
	AllocCountProfileKey string
	AllocBytesProfileKey string
	CPUProfileKey        string
}

func (p *Package) runBenchmark(ctx context.Context, benchTime, benchName string) (*benchmarkResult, error) {
	pprofPath, err := os.MkdirTemp("", "pyrotest-pprof-out")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(pprofPath)

	cpuProfile := filepath.Join(pprofPath, "cpu.pprof")
	memProfile := filepath.Join(pprofPath, "mem.pprof")

	cmd := []string{
		p.testBinary,
		"-test.run", "^$",
		"-test.benchtime", benchTime,
		"-test.bench", benchName,
		"-test.cpuprofile", cpuProfile,
		"-test.memprofile", memProfile,
	}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = p.meta.Dir

	// TODO: Do something with the output
	// TODO: Catch and display errors
	_, err = c.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run benchmark %v: %w", cmd, err)
	}

	result := benchmarkResult{
		ImportPath: p.meta.ImportPath,
		Name:       benchName,
	}

	for _, prof := range []string{cpuProfile, memProfile} {
		f, err := os.Open(prof)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		// TODO: Parse profile values

		res, err := uploadProfile(ctx, p.logger, f)
		if err != nil {
			return nil, err
		}

		for _, sub := range res.SubProfiles {
			switch sub.Name {
			case "allocs":
				result.AllocCountProfileKey = sub.Key
			case "bytes":
				result.AllocBytesProfileKey = sub.Key
			case "cpu":
				result.CPUProfileKey = sub.Key
			}
		}
	}

	return &result, nil

	// TODO: Upload pprof files, before parsing
}

func (p *Package) compileTest(ctx context.Context) error {
	// skip with no test files
	if p.hasNoTests() {
		level.Debug(p.logger).Log("msg", "skipping package as there are no test files")
		return nil
	}

	tmpFile, err := os.CreateTemp("", "pyrotest-test-bin")
	if err != nil {
		return err
	}
	p.testBinary = tmpFile.Name()
	err = tmpFile.Close()
	if err != nil {
		return err
	}

	relativePath, err := filepath.Rel(p.meta.Root, p.meta.Dir)
	if err != nil {
		return err
	}
	relativePath = "./" + relativePath

	cmd := []string{
		"go",
		"test",
		"-trimpath", // needed for reproducible builds
		"-c",        // do not run tests
		"-o", p.testBinary,
		relativePath,
	}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = p.meta.Root
	msg, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to compile test %v error=%s: %w", cmd, string(msg), err)
	}

	f, err := os.Open(p.testBinary)
	if err != nil {
		return err
	}
	defer f.Close()

	stat, err := os.Stat(p.testBinary)
	if err != nil {
		return err
	}
	if stat.Size() == 0 {
		return fmt.Errorf("test binary is empty: %s", p.testBinary)
	}

	hasher := sha256.New()
	_, err = io.Copy(hasher, f)
	if err != nil {
		return err
	}
	p.testBinaryHash = hasher.Sum(nil)
	level.Debug(p.logger).Log("msg", "compiled test binary", "path", p.testBinary, "hash", fmt.Sprintf("%x", p.testBinaryHash))

	return nil
}

func discoverPackages(ctx context.Context, logger log.Logger, workdir string) ([]Package, error) {
	cmd := []string{"go", "list", "-json", "./..."}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = workdir
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}

	err = c.Start()
	if err != nil {
		return nil, err
	}

	var packages []Package
	dec := json.NewDecoder(out)
	for {
		var m packageMeta
		err := dec.Decode(&m)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, err
			}
			break
		}
		packages = append(packages, Package{
			logger: log.With(logger, "package", m.ImportPath),
			meta:   &m,
		})
	}

	err = c.Wait()
	if err != nil {
		return nil, fmt.Errorf("error running %v: %w", cmd, err)
	}

	return packages, nil
}
