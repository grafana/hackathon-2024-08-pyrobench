package bench

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/google/pprof/profile"
	"golang.org/x/perf/benchfmt"
)

type Package struct {
	logger log.Logger

	meta *packageMeta

	testBinary     string
	testBinaryHash []byte
	benchmarkNames []benchmarkMeta
}

type benchmarkMeta struct {
	Name     string
	position *token.Position
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
		p.benchmarkNames = append(p.benchmarkNames, benchmarkMeta{Name: scanner.Text()})
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	return c.Wait()
}

func isBenchmarkNode(n ast.Node) (*ast.FuncDecl, bool) {

	m, ok := n.(*ast.FuncDecl)
	if ok {
		if len(m.Type.Params.List) == 1 {

			if ok {

				star, ok := m.Type.Params.List[0].Type.(*ast.StarExpr)
				if ok {
					sel, ok := star.X.(*ast.SelectorExpr)
					if ok {
						if sel.Sel.Name == "B" && sel.X.(*ast.Ident).Name == "testing" {
							return m, true
						}
					}
				}
			}
		}
	}

	return nil, false
}

func (p *Package) listBenchmarksAst(_ context.Context, filters []*BenchmarkFilter) error {
	if p.hasNoTests() {
		return nil
	}

	//Create a FileSet to work with
	fset := token.NewFileSet()
	//Parse the file and create an AST

	for _, fileName := range p.meta.TestGoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(p.meta.Dir, fileName), nil, parser.ParseComments)
		if err != nil {
			return err
		}

		ast.Inspect(file, func(n ast.Node) bool {
			m, ok := isBenchmarkNode(n)
			if ok {
				keep := true
				if len(filters) > 0 {
					keep = false
					for _, filter := range filters {
						if filter.Filter.MatchString(m.Name.Name) {
							keep = true
							break
						}
					}
				}

				if keep {
					position := fset.Position(m.Pos())
					p.benchmarkNames = append(p.benchmarkNames, benchmarkMeta{
						Name:     m.Name.Name,
						position: &position,
					})
				}
			}
			return true
		})
	}

	return nil
}

type profileResult struct {
	Key              string
	Total            int64
	FlameGraphComURL string
}

type benchmarkResult struct {
	ImportPath string
	Name       string

	AllocObjects profileResult
	AllocSpace   profileResult
	CPU          profileResult

	RawResult []*benchfmt.Result
	Units     benchfmt.UnitMetadataMap
}

func sumProfiles(p *profile.Profile, typeIdx int) int64 {
	var sum int64
	for _, sample := range p.Sample {
		sum += sample.Value[typeIdx]
	}
	return sum
}

func (p *Package) runBenchmark(ctx context.Context, benchTime string, benchCount uint16, benchName string) (*benchmarkResult, error) {
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
		"-test.count", strconv.FormatUint(uint64(benchCount), 10),
		"-test.benchtime", benchTime,
		"-test.bench", regexp.QuoteMeta(benchName),
		"-test.cpuprofile", cpuProfile,
		"-test.memprofile", memProfile,
		"-test.benchmem",
	}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = p.meta.Dir

	bufOut := new(bytes.Buffer)
	bufErr := new(bytes.Buffer)
	c.Stdout = bufOut
	c.Stderr = bufErr

	err = c.Run()
	if err != nil {
		return nil, fmt.Errorf("failed to run benchmark %v stdErr=%s : %w", cmd, bufErr.String(), err)
	}

	results := []*benchfmt.Result{}
	benchReader := benchfmt.NewReader(bytes.NewReader(bufOut.Bytes()), "")
	for benchReader.Scan() {
		// TODO: Pass on the benchmark information to reporter
		b := benchReader.Result()
		result, ok := b.(*benchfmt.Result)
		if !ok {
			continue
		}

		result2 := result.Clone()
		result2.SetConfig("name", benchName)
		results = append(results, result2)
	}
	if err := benchReader.Err(); err != nil {
		return nil, fmt.Errorf("unable to parse benchmark output: %w", benchReader.Err())
	}

	result := benchmarkResult{
		ImportPath: p.meta.ImportPath,
		Name:       benchName,
		RawResult:  results,
		Units:      benchReader.Units(),
	}

	for _, profPath := range []string{cpuProfile, memProfile} {
		profF, err := os.Open(profPath)
		if err != nil {
			return nil, err
		}
		defer profF.Close()

		// TODO: Parse profile values
		prof, err := profile.Parse(profF)
		if err != nil {
			return nil, err
		}

		// find the sub-profiles in the types
		for idx, t := range prof.SampleType {
			switch t.Type {
			case "alloc_objects":
				result.AllocObjects.Total = sumProfiles(prof, idx)
			case "alloc_space":
				result.AllocSpace.Total = sumProfiles(prof, idx)
			case "cpu":
				result.CPU.Total = sumProfiles(prof, idx)
			}
		}

		// rewind file for upload
		_, err = profF.Seek(0, 0)
		if err != nil {
			return nil, err
		}

		res, err := uploadProfile(ctx, p.logger, profF)
		if err != nil {
			return nil, err
		}

		for _, sub := range res.SubProfiles {
			switch sub.Name {
			case "alloc_objects":
				result.AllocObjects.Key = sub.Key

				// TODO(bryan) Link to specific sub-profile. For some reason
				// flamegraph.com does not like building links directly to
				// sub-profiles.
				result.AllocObjects.FlameGraphComURL = res.URL
			case "alloc_space":
				result.AllocSpace.Key = sub.Key
				result.AllocSpace.FlameGraphComURL = res.URL
			case "cpu":
				result.CPU.Key = sub.Key
				result.CPU.FlameGraphComURL = res.URL
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
	cleanupFromContext(ctx)(func() error {
		return os.Remove(tmpFile.Name())
	})

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
