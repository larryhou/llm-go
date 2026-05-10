package builtin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/larryhou/llm-go/tool"
)

const (
	grepMaxResults  = 100
	grepMaxLineLen  = 2000
)

// GrepTool searches file contents using regular expressions via ripgrep.
// Aligned with packages/opencode/src/tool/grep.ts.
type GrepTool struct {
	WorkDir string
}

func (t *GrepTool) Name() string { return "grep" }

func (t *GrepTool) Description() string {
	return `Fast content search tool that works with any codebase size.
Searches file contents using regular expressions.
Supports full regex syntax (e.g. "log.*Error", "function\s+\w+").
Filter files by pattern with the include parameter (e.g. "*.js", "*.{ts,tsx}").
Returns file paths and line numbers with at least one match sorted by modification time.`
}

func (t *GrepTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "The regex pattern to search for in file contents.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Directory or file to search in. Defaults to the current working directory.",
			},
			"include": map[string]any{
				"type":        "string",
				"description": `File glob pattern to restrict search (e.g. "*.go", "*.{ts,tsx}").`,
			},
		},
		"required": []string{"pattern"},
	}
}

func (t *GrepTool) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
	pattern, _ := input["pattern"].(string)
	if pattern == "" {
		return tool.Result{}, tool.Fail("pattern is required")
	}

	searchPath := t.WorkDir
	if p, ok := input["path"].(string); ok && p != "" {
		if filepath.IsAbs(p) {
			searchPath = p
		} else {
			base := t.WorkDir
			if base == "" {
				base, _ = os.Getwd()
			}
			searchPath = filepath.Join(base, p)
		}
	}
	if searchPath == "" {
		searchPath, _ = os.Getwd()
	}

	include, _ := input["include"].(string)

	type match struct {
		file  string
		line  int
		text  string
		mtime int64
	}

	var matches []match

	rgPath, rgErr := exec.LookPath("rg")
	if rgErr != nil {
		return tool.Result{}, tool.Fail("ripgrep (rg) is required for grep but was not found in PATH")
	}

	args := []string{
		"--line-number",
		"--no-heading",
		"--color=never",
		"--with-filename",
	}
	if include != "" {
		args = append(args, "--glob", include)
	}
	args = append(args, pattern, searchPath)

	cmd := exec.CommandContext(ctx, rgPath, args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// No matches — mirrors opencode's `empty` return.
			return tool.Result{
				Title:  pattern,
				Output: "No files found",
				Metadata: map[string]any{"matches": 0, "truncated": false},
			}, nil
		}
		return tool.Result{}, tool.Fail(fmt.Sprintf("grep failed: %v", err))
	}

	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		// Format: file:linenum:text
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		file := parts[0]
		lineNum := 0
		fmt.Sscanf(parts[1], "%d", &lineNum)
		text := parts[2]
		if len(text) > grepMaxLineLen {
			text = text[:grepMaxLineLen] + "..."
		}
		matches = append(matches, match{file: file, line: lineNum, text: text})
	}

	total := len(matches)
	truncated := total > grepMaxResults
	final := matches
	if truncated {
		final = matches[:grepMaxResults]
	}

	// Fetch mtime per unique file.
	mtimes := make(map[string]int64)
	var mtimeMu sync.Mutex
	var mtimeWg sync.WaitGroup
	sem := make(chan struct{}, 16)
	seen := make(map[string]bool)
	for _, m := range final {
		if seen[m.file] {
			continue
		}
		seen[m.file] = true
		mtimeWg.Add(1)
		sem <- struct{}{}
		go func(f string) {
			defer mtimeWg.Done()
			defer func() { <-sem }()
			var mtime int64
			if fi, err2 := os.Stat(f); err2 == nil {
				mtime = fi.ModTime().UnixNano()
			}
			mtimeMu.Lock()
			mtimes[f] = mtime
			mtimeMu.Unlock()
		}(m.file)
	}
	mtimeWg.Wait()

	// Sort by mtime descending.
	sort.SliceStable(final, func(i, j int) bool {
		return mtimes[final[i].file] > mtimes[final[j].file]
	})

	// Build output — mirrors opencode grep.ts output assembly.
	lines := []string{fmt.Sprintf("Found %d matches%s", total, func() string {
		if truncated {
			return fmt.Sprintf(" (showing first %d)", grepMaxResults)
		}
		return ""
	}())}

	current := ""
	for _, m := range final {
		if current != m.file {
			if current != "" {
				lines = append(lines, "")
			}
			current = m.file
			lines = append(lines, m.file+":")
		}
		lines = append(lines, fmt.Sprintf("  Line %d: %s", m.line, m.text))
	}

	if truncated {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf(
			"(Results truncated: showing %d of %d matches (%d hidden). Consider using a more specific path or pattern.)",
			grepMaxResults, total, total-grepMaxResults,
		))
	}

	return tool.Result{
		Title:  pattern,
		Output: strings.Join(lines, "\n"),
		Metadata: map[string]any{
			"matches":   total,
			"truncated": truncated,
		},
	}, nil
}
