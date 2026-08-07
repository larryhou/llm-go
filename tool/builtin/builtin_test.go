package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func ctx() context.Context { return context.Background() }

func mustTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "builtin-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ═══════════════════════════════════════════════════════════════════════════
// GlobTool
// ═══════════════════════════════════════════════════════════════════════════

func TestGlob_MatchesFiles(t *testing.T) {
	dir := mustTempDir(t)
	writeFile(t, filepath.Join(dir, "a.go"), "")
	writeFile(t, filepath.Join(dir, "b.go"), "")
	writeFile(t, filepath.Join(dir, "c.txt"), "")

	tool := &GlobTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"pattern": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "No files found") {
		t.Fatal("expected matches, got 'No files found'")
	}
	if !strings.Contains(res.Output, "a.go") || !strings.Contains(res.Output, "b.go") {
		t.Errorf("expected a.go and b.go in output, got:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "c.txt") {
		t.Errorf("c.txt should not match *.go")
	}
}

func TestGlob_NoMatches(t *testing.T) {
	dir := mustTempDir(t)
	tool := &GlobTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"pattern": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "No files found" {
		t.Errorf("expected 'No files found', got: %q", res.Output)
	}
}

func TestGlob_PathMustBeDirectory(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "file.txt")
	writeFile(t, f, "")

	tool := &GlobTool{WorkDir: dir}
	_, err := tool.Execute(ctx(), map[string]any{"pattern": "*.go", "path": f})
	if err == nil {
		t.Fatal("expected error for file path, got nil")
	}
}

func TestGlob_Truncation(t *testing.T) {
	dir := mustTempDir(t)
	for i := 0; i <= globLimit; i++ {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%03d.go", i)), "")
	}
	tool := &GlobTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"pattern": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "too large") && !strings.Contains(res.Output, "truncated") {
		t.Errorf("expected truncation notice, got:\n%s", res.Output)
	}
	if res.Metadata["truncated"] != true {
		t.Errorf("expected metadata truncated=true")
	}
}

func TestGlob_MissingPattern(t *testing.T) {
	tool := &GlobTool{}
	_, err := tool.Execute(ctx(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing pattern")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// GrepTool
// ═══════════════════════════════════════════════════════════════════════════

func TestGrep_FindsMatches(t *testing.T) {
	dir := mustTempDir(t)
	writeFile(t, filepath.Join(dir, "a.go"), "package main\nfunc hello() {}\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package main\nfunc world() {}\n")

	tool := &GrepTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"pattern": "func"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "No files found") {
		t.Fatalf("expected matches, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "Found") {
		t.Errorf("expected 'Found N matches' header, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "Line") {
		t.Errorf("expected 'Line N:' entries, got:\n%s", res.Output)
	}
}

func TestGrep_NoMatches(t *testing.T) {
	dir := mustTempDir(t)
	writeFile(t, filepath.Join(dir, "a.go"), "package main\n")

	tool := &GrepTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"pattern": "XYZNOTFOUND"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "No files found" {
		t.Errorf("expected 'No files found', got: %q", res.Output)
	}
}

func TestGrep_GroupsByFile(t *testing.T) {
	dir := mustTempDir(t)
	writeFile(t, filepath.Join(dir, "a.go"), "hello\nhello\n")

	tool := &GrepTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"pattern": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	// File header should appear once.
	count := strings.Count(res.Output, "a.go:")
	if count != 1 {
		t.Errorf("expected file header once, got %d occurrences in:\n%s", count, res.Output)
	}
}

func TestGrep_TitleIsPattern(t *testing.T) {
	dir := mustTempDir(t)
	tool := &GrepTool{WorkDir: dir}
	res, _ := tool.Execute(ctx(), map[string]any{"pattern": "mypattern"})
	if res.Title != "mypattern" {
		t.Errorf("expected Title=mypattern, got %q", res.Title)
	}
}

func TestGrep_IncludeFilter(t *testing.T) {
	dir := mustTempDir(t)
	writeFile(t, filepath.Join(dir, "a.go"), "hello\n")
	writeFile(t, filepath.Join(dir, "b.txt"), "hello\n")

	tool := &GrepTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"pattern": "hello", "include": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "b.txt") {
		t.Errorf("b.txt should be excluded by include filter")
	}
}

func TestGrep_MissingPattern(t *testing.T) {
	tool := &GrepTool{}
	_, err := tool.Execute(ctx(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing pattern")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// ReadTool
// ═══════════════════════════════════════════════════════════════════════════

func TestRead_TextFile(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "hello.txt")
	writeFile(t, f, "line one\nline two\nline three\n")

	tool := &ReadTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"filePath": f})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "<path>") {
		t.Errorf("expected <path> tag, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "<content>") {
		t.Errorf("expected <content> tag, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "1: line one") {
		t.Errorf("expected numbered lines, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "End of file - total 3 lines") {
		t.Errorf("expected end-of-file footer, got:\n%s", res.Output)
	}
}

func TestRead_Offset(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "f.txt")
	writeFile(t, f, "a\nb\nc\nd\n")

	tool := &ReadTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"filePath": f, "offset": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "1: a") {
		t.Errorf("offset=3 should skip line 1, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "3: c") {
		t.Errorf("expected line 3 in output, got:\n%s", res.Output)
	}
}

func TestRead_Limit(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "f.txt")
	writeFile(t, f, "a\nb\nc\nd\ne\n")

	tool := &ReadTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"filePath": f, "limit": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "3: c") {
		t.Errorf("limit=2 should exclude line 3+, got:\n%s", res.Output)
	}
	// Should show "Showing lines" footer since more lines exist.
	if !strings.Contains(res.Output, "Showing lines") {
		t.Errorf("expected 'Showing lines' footer, got:\n%s", res.Output)
	}
}

func TestRead_Directory(t *testing.T) {
	dir := mustTempDir(t)
	writeFile(t, filepath.Join(dir, "a.go"), "")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	tool := &ReadTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"filePath": dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "<type>directory</type>") {
		t.Errorf("expected directory type tag, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "sub/") {
		t.Errorf("expected sub/ with trailing slash, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "a.go") {
		t.Errorf("expected a.go in listing, got:\n%s", res.Output)
	}
}

func TestRead_FileNotFound(t *testing.T) {
	tool := &ReadTool{}
	_, err := tool.Execute(ctx(), map[string]any{"filePath": "/nonexistent/file.txt"})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestRead_BinaryExtRejected(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "test.exe")
	writeFile(t, f, "some content")

	tool := &ReadTool{WorkDir: dir}
	_, err := tool.Execute(ctx(), map[string]any{"filePath": f})
	if err == nil {
		t.Fatal("expected error for binary extension")
	}
	if !strings.Contains(err.Error(), "Cannot read binary file") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRead_LineTruncation(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "long.txt")
	longLine := strings.Repeat("x", readMaxLineLen+100)
	writeFile(t, f, longLine+"\n")

	tool := &ReadTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"filePath": f})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "line truncated to") {
		t.Errorf("expected line truncation notice, got:\n%s", res.Output)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// WriteTool
// ═══════════════════════════════════════════════════════════════════════════

func TestWrite_CreatesFile(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "new.txt")

	tool := &WriteTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"filePath": f, "content": "hello\nworld\n"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "Wrote file successfully." {
		t.Errorf("expected 'Wrote file successfully.', got: %q", res.Output)
	}
	got := readFile(t, f)
	if got != "hello\nworld\n" {
		t.Errorf("unexpected file content: %q", got)
	}
}

func TestWrite_OverwritesFile(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "existing.txt")
	writeFile(t, f, "old content")

	tool := &WriteTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{"filePath": f, "content": "new content"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "Wrote file successfully." {
		t.Errorf("expected 'Wrote file successfully.', got: %q", res.Output)
	}
	got := readFile(t, f)
	if got != "new content" {
		t.Errorf("unexpected file content: %q", got)
	}
}

func TestWrite_CreatesIntermediateDirs(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "a", "b", "c.txt")

	tool := &WriteTool{WorkDir: dir}
	_, err := tool.Execute(ctx(), map[string]any{"filePath": f, "content": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestWrite_MissingFilePath(t *testing.T) {
	tool := &WriteTool{}
	_, err := tool.Execute(ctx(), map[string]any{"content": "hi"})
	if err == nil {
		t.Fatal("expected error for missing filePath")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// EditTool
// ═══════════════════════════════════════════════════════════════════════════

func TestEdit_SimpleReplace(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "f.go")
	writeFile(t, f, "func hello() {}\nfunc world() {}\n")

	tool := &EditTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{
		"filePath":  f,
		"oldString": "func hello() {}",
		"newString": "func greet() {}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "Edit applied successfully." {
		t.Errorf("unexpected output: %q", res.Output)
	}
	got := readFile(t, f)
	if !strings.Contains(got, "func greet() {}") {
		t.Errorf("replacement not applied, got:\n%s", got)
	}
	if strings.Contains(got, "func hello() {}") {
		t.Errorf("old string still present, got:\n%s", got)
	}
}

func TestEdit_ReplaceAll(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "f.txt")
	writeFile(t, f, "foo\nfoo\nfoo\n")

	tool := &EditTool{WorkDir: dir}
	_, err := tool.Execute(ctx(), map[string]any{
		"filePath":   f,
		"oldString":  "foo",
		"newString":  "bar",
		"replaceAll": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, f)
	if strings.Contains(got, "foo") {
		t.Errorf("expected all 'foo' replaced, got:\n%s", got)
	}
	if strings.Count(got, "bar") != 3 {
		t.Errorf("expected 3 'bar' replacements, got:\n%s", got)
	}
}

func TestEdit_AmbiguousOldString(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "f.txt")
	writeFile(t, f, "foo\nfoo\n")

	tool := &EditTool{WorkDir: dir}
	_, err := tool.Execute(ctx(), map[string]any{
		"filePath":  f,
		"oldString": "foo",
		"newString": "bar",
	})
	if err == nil {
		t.Fatal("expected error for ambiguous match")
	}
	if !strings.Contains(err.Error(), "multiple matches") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEdit_OldStringNotFound(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "f.txt")
	writeFile(t, f, "hello world\n")

	tool := &EditTool{WorkDir: dir}
	_, err := tool.Execute(ctx(), map[string]any{
		"filePath":  f,
		"oldString": "NOTEXIST",
		"newString": "x",
	})
	if err == nil {
		t.Fatal("expected error for not-found oldString")
	}
	if !strings.Contains(err.Error(), "Could not find") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEdit_IdenticalStrings(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "f.txt")
	writeFile(t, f, "hello\n")

	tool := &EditTool{WorkDir: dir}
	_, err := tool.Execute(ctx(), map[string]any{
		"filePath":  f,
		"oldString": "hello",
		"newString": "hello",
	})
	if err == nil {
		t.Fatal("expected error for identical strings")
	}
}

func TestEdit_EmptyOldStringCreatesFile(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "new.txt")

	tool := &EditTool{WorkDir: dir}
	res, err := tool.Execute(ctx(), map[string]any{
		"filePath":  f,
		"oldString": "",
		"newString": "created content",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "Edit applied successfully." {
		t.Errorf("unexpected output: %q", res.Output)
	}
	got := readFile(t, f)
	if got != "created content" {
		t.Errorf("unexpected file content: %q", got)
	}
}

func TestEdit_LineTrimmedFuzzy(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "f.go")
	writeFile(t, f, "func foo() {\n\treturn nil\n}\n")

	tool := &EditTool{WorkDir: dir}
	// oldString has different indentation — should match via lineTrimmedReplacer.
	_, err := tool.Execute(ctx(), map[string]any{
		"filePath":  f,
		"oldString": "func foo() {\n    return nil\n}",
		"newString": "func foo() {\n\treturn 42\n}",
	})
	if err != nil {
		t.Fatalf("lineTrimmed fuzzy match failed: %v", err)
	}
	got := readFile(t, f)
	if !strings.Contains(got, "42") {
		t.Errorf("replacement not applied, got:\n%s", got)
	}
}

func TestEdit_CRLFPreserved(t *testing.T) {
	dir := mustTempDir(t)
	f := filepath.Join(dir, "f.txt")
	writeFile(t, f, "hello\r\nworld\r\n")

	tool := &EditTool{WorkDir: dir}
	_, err := tool.Execute(ctx(), map[string]any{
		"filePath":  f,
		"oldString": "hello",
		"newString": "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, f)
	if !strings.Contains(got, "\r\n") {
		t.Errorf("CRLF line endings not preserved, got: %q", got)
	}
}

func TestEdit_FileNotFound(t *testing.T) {
	tool := &EditTool{}
	_, err := tool.Execute(ctx(), map[string]any{
		"filePath":  "/nonexistent/file.txt",
		"oldString": "x",
		"newString": "y",
	})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// editReplace internals
// ═══════════════════════════════════════════════════════════════════════════

func TestEditReplace_Simple(t *testing.T) {
	out, err := editReplace("hello world", "hello", "hi", false)
	if err != nil {
		t.Fatal(err)
	}
	if out != "hi world" {
		t.Errorf("got %q", out)
	}
}

func TestEditReplace_MultipleAmbiguous(t *testing.T) {
	_, err := editReplace("foo foo", "foo", "bar", false)
	if err == nil || !strings.Contains(err.Error(), "multiple matches") {
		t.Errorf("expected multiple-matches error, got: %v", err)
	}
}

func TestEditReplace_ReplaceAll(t *testing.T) {
	out, err := editReplace("foo foo foo", "foo", "bar", true)
	if err != nil {
		t.Fatal(err)
	}
	if out != "bar bar bar" {
		t.Errorf("got %q", out)
	}
}

func TestEditReplace_BlockAnchor(t *testing.T) {
	content := "func a() {\n\tx := 1\n\treturn x\n}\n"
	// oldString uses different middle line whitespace — blockAnchor should match.
	old := "func a() {\n  x := 1\n  return x\n}"
	out, err := editReplace(content, old, "func a() { return 42 }", false)
	if err != nil {
		t.Fatalf("blockAnchor failed: %v", err)
	}
	if !strings.Contains(out, "return 42") {
		t.Errorf("replacement not in output: %q", out)
	}
}

func TestEditReplace_EscapeNormalized(t *testing.T) {
	content := "say hello\nworld\n"
	// oldString uses \n escape sequence.
	old := "say hello\\nworld"
	out, err := editReplace(content, old, "say hi\nplanet", false)
	if err != nil {
		t.Fatalf("escapeNormalized failed: %v", err)
	}
	if !strings.Contains(out, "hi") {
		t.Errorf("replacement not in output: %q", out)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// ShellTool
// ═══════════════════════════════════════════════════════════════════════════

func TestShell_SimpleCommand(t *testing.T) {
	tool := &ShellTool{}
	res, err := tool.Execute(ctx(), map[string]any{
		"command":     "echo hello",
		"description": "echo hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("expected 'hello' in output, got:\n%s", res.Output)
	}
}

func TestShell_ExitCode(t *testing.T) {
	tool := &ShellTool{}
	res, err := tool.Execute(ctx(), map[string]any{
		"command":     "exit 42",
		"description": "exit with code",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metadata["exit"] != 42 {
		t.Errorf("expected exit code 42, got: %v", res.Metadata["exit"])
	}
}

func TestShell_NoOutput(t *testing.T) {
	tool := &ShellTool{}
	res, err := tool.Execute(ctx(), map[string]any{
		"command":     "true",
		"description": "no output command",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "(no output)") {
		t.Errorf("expected '(no output)', got:\n%s", res.Output)
	}
}

func TestShell_Workdir(t *testing.T) {
	dir := mustTempDir(t)
	tool := &ShellTool{}
	res, err := tool.Execute(ctx(), map[string]any{
		"command":     "pwd",
		"description": "print working dir",
		"workdir":     dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	// On macOS /tmp is symlinked to /private/tmp, normalise for comparison.
	got := strings.TrimSpace(res.Output)
	// Remove trailing shell_metadata block if present.
	if idx := strings.Index(got, "\n<shell_metadata>"); idx >= 0 {
		got = strings.TrimSpace(got[:idx])
	}
	real, _ := filepath.EvalSymlinks(dir)
	if got != real && got != dir {
		t.Errorf("expected workdir %q or %q, got %q", dir, real, got)
	}
}

func TestShell_Timeout(t *testing.T) {
	tool := &ShellTool{}
	res, err := tool.Execute(ctx(), map[string]any{
		"command":     "sleep 10",
		"description": "sleep",
		"timeout":     float64(100), // 100ms
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metadata["timed_out"] != true {
		t.Errorf("expected timed_out=true, got: %v", res.Metadata["timed_out"])
	}
	if !strings.Contains(res.Output, "shell_metadata") {
		t.Errorf("expected shell_metadata block, got:\n%s", res.Output)
	}
}

func TestShell_NegativeTimeoutError(t *testing.T) {
	tool := &ShellTool{}
	_, err := tool.Execute(ctx(), map[string]any{
		"command":     "echo hi",
		"description": "test",
		"timeout":     float64(-1),
	})
	if err == nil {
		t.Fatal("expected error for negative timeout")
	}
}

func TestShell_MissingCommand(t *testing.T) {
	tool := &ShellTool{}
	_, err := tool.Execute(ctx(), map[string]any{"description": "test"})
	if err == nil {
		t.Fatal("expected error for missing command")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// tailOutput
// ═══════════════════════════════════════════════════════════════════════════

func TestTailOutput_NoTruncation(t *testing.T) {
	s := "a\nb\nc"
	r := tailOutput(s, 100, 10000)
	if r.cut {
		t.Error("expected no cut")
	}
	if r.text != s {
		t.Errorf("got %q", r.text)
	}
}

func TestTailOutput_LineLimit(t *testing.T) {
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i)
	}
	s := strings.Join(lines, "\n")
	r := tailOutput(s, 5, 100000)
	if !r.cut {
		t.Error("expected cut")
	}
	outLines := strings.Split(r.text, "\n")
	if len(outLines) > 5 {
		t.Errorf("expected at most 5 lines, got %d", len(outLines))
	}
	// Should contain last lines.
	if !strings.Contains(r.text, "line19") {
		t.Errorf("expected last line in output, got:\n%s", r.text)
	}
}

func TestTailOutput_ByteLimit(t *testing.T) {
	// 3 lines each ~20 bytes; byte limit = 25 → should cut.
	s := "abcdefghijklmnopqrst\nabcdefghijklmnopqrst\nabcdefghijklmnopqrst"
	r := tailOutput(s, 100, 25)
	if !r.cut {
		t.Error("expected cut due to byte limit")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// levenshtein
// ═══════════════════════════════════════════════════════════════════════════

func TestLevenshtein(t *testing.T) {
	cases := []struct{ a, b string; want int }{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"kitten", "sitting", 3},
	}
	for _, c := range cases {
		got := levenshtein(c.a, c.b)
		if got != c.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
