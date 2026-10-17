// Command gohook is a Claude Code PostToolUse hook for Go.
//
// After Claude edits a .go file, gohook checks the file's package for gofmt
// formatting, lint issues (golangci-lint, or go vet as a fallback), passing
// tests, and a minimum statement coverage. If any check fails it prints a
// compact report to stderr and exits 2, which Claude Code feeds back to Claude
// so it can fix the problem right away.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Exit codes understood by Claude Code hooks.
const (
	exitOK    = 0 // nothing to report
	exitError = 1 // gohook itself failed; shown to the user, not Claude
	exitBlock = 2 // checks failed; stderr is fed back to Claude
)

type hookInput struct {
	Cwd       string `json:"cwd"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

type config struct {
	minCoverage float64
	skip        []string
	fix         bool
	timeout     time.Duration
	maxLines    int
}

type section struct {
	title string
	lines []string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stderr))
}

func run(args []string, stdin io.Reader, stderr io.Writer) int {
	cfg, err := parseConfig(args, os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gohook: %v\n", err)
		return exitError
	}

	var in hookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		_, _ = fmt.Fprintf(stderr, "gohook: reading hook input: %v\n", err)
		return exitError
	}

	file := in.ToolInput.FilePath
	if !strings.HasSuffix(file, ".go") {
		return exitOK
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(in.Cwd, file)
	}
	if _, err := os.Stat(file); err != nil {
		return exitOK // deleted or moved; nothing to check
	}

	root := findModuleRoot(filepath.Dir(file))
	if root == "" {
		return exitOK // not in a Go module
	}
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return exitOK
	}
	rel = filepath.ToSlash(rel)
	if cfg.skipped(rel) || isGenerated(file) {
		return exitOK
	}

	pkgDir := filepath.Dir(file)
	pkgArg := packageArg(root, pkgDir)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	var failures []section
	var notes []string

	fmtFail, fmtNote := checkFormat(ctx, cfg, file)
	if fmtFail != nil {
		failures = append(failures, *fmtFail)
	}
	if fmtNote != "" {
		notes = append(notes, fmtNote)
	}
	if s := checkLint(ctx, cfg, root, pkgArg); s != nil {
		failures = append(failures, *s)
	}
	if s := checkCoverage(ctx, cfg, root, pkgDir, pkgArg, file); s != nil {
		failures = append(failures, *s)
	}

	if len(failures) == 0 {
		return exitOK
	}
	writeReport(stderr, rel, pkgArg, failures, notes)
	return exitBlock
}

func parseConfig(args []string, getenv func(string) string) (config, error) {
	cfg := config{minCoverage: 90, timeout: 3 * time.Minute, maxLines: 60}

	if v := getenv("GOHOOK_MIN_COVERAGE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("GOHOOK_MIN_COVERAGE: %w", err)
		}
		cfg.minCoverage = f
	}
	if v := getenv("GOHOOK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("GOHOOK_TIMEOUT: %w", err)
		}
		cfg.timeout = d
	}
	cfg.fix = getenv("GOHOOK_FIX") == "1" || getenv("GOHOOK_FIX") == "true"
	skip := getenv("GOHOOK_SKIP")

	fs := flag.NewFlagSet("gohook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Float64Var(&cfg.minCoverage, "min", cfg.minCoverage, "minimum package statement coverage, in percent")
	fs.StringVar(&skip, "skip", skip, "comma-separated paths to skip (path.Match patterns, or dir/... for a subtree)")
	fs.BoolVar(&cfg.fix, "fix", cfg.fix, "rewrite unformatted files with gofmt instead of reporting them")
	fs.DurationVar(&cfg.timeout, "timeout", cfg.timeout, "time limit for all checks")
	fs.IntVar(&cfg.maxLines, "max-lines", cfg.maxLines, "maximum report lines per check")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cfg.minCoverage < 0 || cfg.minCoverage > 100 {
		return cfg, fmt.Errorf("-min must be between 0 and 100, got %v", cfg.minCoverage)
	}
	for _, p := range strings.Split(skip, ",") {
		if p = strings.TrimSpace(p); p != "" {
			cfg.skip = append(cfg.skip, p)
		}
	}
	return cfg, nil
}

// skipped reports whether a module-relative, slash-separated path is excluded.
func (c config) skipped(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if seg == "vendor" || seg == "testdata" {
			return true
		}
	}
	for _, p := range c.skip {
		if p == "..." {
			return true
		}
		if dir, ok := strings.CutSuffix(p, "/..."); ok {
			if rel == dir || strings.HasPrefix(rel, dir+"/") {
				return true
			}
			continue
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		if ok, _ := path.Match(p, path.Base(rel)); ok {
			return true
		}
	}
	return false
}

func findModuleRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func packageArg(root, pkgDir string) string {
	rel, err := filepath.Rel(root, pkgDir)
	if err != nil || rel == "." {
		return "."
	}
	return "./" + filepath.ToSlash(rel)
}

// isGenerated follows the Go convention: a "Code generated ... DO NOT EDIT."
// comment before the package clause.
func isGenerated(file string) bool {
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			return false
		}
		if strings.HasPrefix(line, "// Code generated ") && strings.HasSuffix(line, " DO NOT EDIT.") {
			return true
		}
	}
	return false
}

func runCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("%s timed out", name)
	}
	return string(out), err
}

func checkFormat(ctx context.Context, cfg config, file string) (*section, string) {
	dir := filepath.Dir(file)
	out, err := runCmd(ctx, dir, "gofmt", "-l", file)
	if err != nil {
		return &section{"gofmt", append([]string{"gofmt failed, most likely a syntax error:"},
			head(splitLines(out), cfg.maxLines)...)}, ""
	}
	if strings.TrimSpace(out) == "" {
		return nil, ""
	}
	if cfg.fix {
		if out, err := runCmd(ctx, dir, "gofmt", "-w", file); err != nil {
			return &section{"gofmt", append([]string{"gofmt -w failed:"}, head(splitLines(out), cfg.maxLines)...)}, ""
		}
		return nil, "gofmt reformatted " + filepath.Base(file) + "; re-read it before editing again."
	}
	diff, _ := runCmd(ctx, dir, "gofmt", "-d", file)
	return &section{"gofmt", append([]string{"File is not gofmt-formatted. Run gofmt -w on it, or apply:"},
		head(splitLines(diff), cfg.maxLines)...)}, ""
}

func checkLint(ctx context.Context, cfg config, root, pkgArg string) *section {
	if _, err := exec.LookPath("golangci-lint"); err == nil {
		out, err := runCmd(ctx, root, "golangci-lint", "run", pkgArg)
		if err == nil {
			return nil
		}
		return &section{"golangci-lint", head(splitLines(out), cfg.maxLines)}
	}
	out, err := runCmd(ctx, root, "go", "vet", pkgArg)
	if err == nil {
		return nil
	}
	return &section{"go vet (golangci-lint not installed)", head(splitLines(out), cfg.maxLines)}
}

func checkCoverage(ctx context.Context, cfg config, root, pkgDir, pkgArg, edited string) *section {
	if cfg.minCoverage == 0 {
		return nil
	}
	tmp, err := os.CreateTemp("", "gohook-*.cov")
	if err != nil {
		return &section{"coverage", []string{"could not create a temp file: " + err.Error()}}
	}
	prof := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(prof) }()

	if tests, _ := filepath.Glob(filepath.Join(pkgDir, "*_test.go")); len(tests) == 0 {
		return &section{"coverage", []string{fmt.Sprintf(
			"%s has no tests, so coverage is 0%% (minimum %s%%). Add a _test.go file in %s.",
			pkgArg, fmtPct(cfg.minCoverage), pkgArg)}}
	}
	out, err := runCmd(ctx, root, "go", "test", "-covermode=set", "-coverprofile="+prof, pkgArg)
	if err != nil {
		return &section{"go test", append([]string{"Tests failed or did not build:"},
			tail(splitLines(out), cfg.maxLines)...)}
	}

	blocks, err := parseProfile(prof)
	if err != nil {
		return &section{"coverage", []string{"could not read coverage profile: " + err.Error()}}
	}
	covered, total := countStatements(blocks)
	if total == 0 {
		return nil // nothing executable, e.g. only types and constants
	}
	pct := 100 * float64(covered) / float64(total)
	if pct >= cfg.minCoverage {
		return nil
	}

	need := int(math.Ceil(cfg.minCoverage/100*float64(total))) - covered
	lines := []string{fmt.Sprintf("Package coverage is %.1f%% (%d/%d statements); the minimum is %s%%. Cover at least %d more statement(s).",
		pct, covered, total, fmtPct(cfg.minCoverage), need)}
	lines = append(lines, "Uncovered code:")
	lines = append(lines, head(uncoveredReport(blocks, pkgDir, filepath.Base(edited)), cfg.maxLines)...)
	return &section{"coverage", lines}
}

type block struct {
	file       string // base name
	start, end int    // line numbers
	stmts      int
	count      int
}

// parseProfile reads a go test -coverprofile file.
func parseProfile(name string) ([]block, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return parseProfileData(string(data))
}

func parseProfileData(data string) ([]block, error) {
	type key struct {
		file       string
		start, end string
	}
	seen := map[key]int{}
	var blocks []block
	for i, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("line %d: malformed: %q", i+1, line)
		}
		colon := strings.LastIndex(f[0], ":")
		if colon < 0 {
			return nil, fmt.Errorf("line %d: no file separator: %q", i+1, line)
		}
		file, span := f[0][:colon], f[0][colon+1:]
		from, to, ok := strings.Cut(span, ",")
		if !ok {
			return nil, fmt.Errorf("line %d: bad span %q", i+1, span)
		}
		start, err1 := lineOf(from)
		end, err2 := lineOf(to)
		stmts, err3 := strconv.Atoi(f[1])
		count, err4 := strconv.Atoi(f[2])
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		k := key{file, from, to}
		if idx, dup := seen[k]; dup {
			blocks[idx].count = max(blocks[idx].count, count)
			continue
		}
		seen[k] = len(blocks)
		blocks = append(blocks, block{file: path.Base(file), start: start, end: end, stmts: stmts, count: count})
	}
	return blocks, nil
}

func lineOf(pos string) (int, error) {
	l, _, _ := strings.Cut(pos, ".")
	return strconv.Atoi(l)
}

func countStatements(blocks []block) (covered, total int) {
	for _, b := range blocks {
		total += b.stmts
		if b.count > 0 {
			covered += b.stmts
		}
	}
	return covered, total
}

// uncoveredReport lists uncovered line ranges per file, labelled with the
// enclosing function. The edited file comes first.
func uncoveredReport(blocks []block, pkgDir, edited string) []string {
	byFile := map[string][][2]int{}
	for _, b := range blocks {
		if b.count == 0 && b.stmts > 0 {
			byFile[b.file] = append(byFile[b.file], [2]int{b.start, b.end})
		}
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		if (files[i] == edited) != (files[j] == edited) {
			return files[i] == edited
		}
		return files[i] < files[j]
	})

	var out []string
	for _, f := range files {
		funcs := funcRanges(filepath.Join(pkgDir, f))
		for _, r := range mergeRanges(byFile[f]) {
			loc := fmt.Sprintf("  %s:%d-%d", f, r[0], r[1])
			if r[0] == r[1] {
				loc = fmt.Sprintf("  %s:%d", f, r[0])
			}
			if name := enclosingFunc(funcs, r[0]); name != "" {
				loc += " in " + name
			}
			out = append(out, loc)
		}
	}
	return out
}

func mergeRanges(rs [][2]int) [][2]int {
	if len(rs) == 0 {
		return nil
	}
	rs = append([][2]int(nil), rs...)
	sort.Slice(rs, func(i, j int) bool { return rs[i][0] < rs[j][0] })
	merged := [][2]int{rs[0]}
	for _, r := range rs[1:] {
		last := &merged[len(merged)-1]
		if r[0] <= last[1]+1 {
			last[1] = max(last[1], r[1])
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

type funcRange struct {
	name       string
	start, end int
}

func funcRanges(file string) []funcRange {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var out []funcRange
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		out = append(out, funcRange{
			name:  funcName(fd),
			start: fset.Position(fd.Pos()).Line,
			end:   fset.Position(fd.End()).Line,
		})
	}
	return out
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	return "(" + typeString(fd.Recv.List[0].Type) + ")." + fd.Name.Name
}

func typeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver T[K]
		return typeString(t.X)
	case *ast.IndexListExpr: // generic receiver T[K, V]
		return typeString(t.X)
	default:
		return "?"
	}
}

func enclosingFunc(funcs []funcRange, line int) string {
	for _, f := range funcs {
		if line >= f.start && line <= f.end {
			return f.name
		}
	}
	return ""
}

func writeReport(w io.Writer, rel, pkgArg string, failures []section, notes []string) {
	var b strings.Builder
	b.WriteString("gohook: checks failed after editing " + rel + " (package " + pkgArg + "). Fix these before moving on.\n")
	for _, s := range failures {
		b.WriteString("\n## " + s.title + "\n")
		for _, l := range s.lines {
			b.WriteString(l + "\n")
		}
	}
	for _, n := range notes {
		b.WriteString("\nnote: " + n + "\n")
	}
	_, _ = io.WriteString(w, b.String())
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func head(lines []string, n int) []string {
	if n <= 0 || len(lines) <= n {
		return lines
	}
	return append(lines[:n:n], fmt.Sprintf("... %d more line(s) omitted", len(lines)-n))
}

func tail(lines []string, n int) []string {
	if n <= 0 || len(lines) <= n {
		return lines
	}
	return append([]string{fmt.Sprintf("... %d earlier line(s) omitted", len(lines)-n)}, lines[len(lines)-n:]...)
}

func fmtPct(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
