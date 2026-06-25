// Package builtin provides standard built-in tools aligned with opencode's
// packages/opencode/src/tool/ implementations.
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

const globLimit = 50

// GlobTool finds files matching a glob pattern, sorted by modification time.
// Aligned with packages/opencode/src/tool/glob.ts.
type GlobTool struct {
	// WorkDir is the default search root when no path is provided.
	WorkDir string
}

func (t *GlobTool) Name() string { return "glob" }

func (t *GlobTool) Description() string {
	return `Fast file pattern matching tool that works with any codebase size.
Supports glob patterns like "**/*.js" or "src/**/*.ts".
Returns matching file paths sorted by modification time.
Use this tool when you need to find files by name patterns.
When you are doing an open-ended search that may require multiple rounds of globbing and grepping, use the Task tool instead.
You have the capability to call multiple tools in a single response. It is always better to speculatively perform multiple searches as a batch that are potentially useful.`
}

func (t *GlobTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "The glob pattern to match files against",
			},
			"path": map[string]any{
				"type":        "string",
				"description": `The directory to search in. If not specified, the current working directory will be used. IMPORTANT: Omit this field to use the default directory. DO NOT enter "undefined" or "null" - simply omit it for the default behavior. Must be a valid directory path if provided.`,
			},
		},
		"required": []string{"pattern"},
	}
}

func (t *GlobTool) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
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

	info, err := os.Stat(searchPath)
	if err != nil {
		return tool.Result{}, tool.Fail(fmt.Sprintf("path does not exist: %s", searchPath))
	}
	if !info.IsDir() {
		return tool.Result{}, tool.Fail(fmt.Sprintf("glob path must be a directory: %s", searchPath))
	}

	// Collect all matching paths (no early cap) so we can write the full list
	// to a file when truncation is needed.
	var rawPaths []string
	if rgPath, err2 := exec.LookPath("rg"); err2 == nil {
		rawPaths, err = runRgGlob(ctx, rgPath, searchPath, pattern, 0)
	} else {
		rawPaths, err = walkGlob(searchPath, pattern, 0)
	}
	if err != nil {
		return tool.Result{}, tool.Fail(fmt.Sprintf("glob failed: %v", err))
	}

	truncated := len(rawPaths) > globLimit

	// Enrich with mtime and sort by most-recently-modified (descending).
	type entry struct {
		path  string
		mtime int64
	}
	entries := make([]entry, 0, len(rawPaths))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, p := range rawPaths {
		wg.Add(1)
		sem <- struct{}{}
		go func(p string) {
			defer wg.Done()
			defer func() { <-sem }()
			var mtime int64
			if fi, err3 := os.Stat(p); err3 == nil {
				mtime = fi.ModTime().UnixNano()
			}
			mu.Lock()
			entries = append(entries, entry{path: p, mtime: mtime})
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].mtime > entries[j].mtime
	})

	// Build output — mirrors opencode glob.ts output assembly.
	var lines []string
	displayed := entries
	var outputPath string
	if len(entries) == 0 {
		lines = append(lines, "No files found")
	} else if truncated {
		// Write full path list to a temp file; return only the first globLimit
		// entries plus a hint so the LLM can explore via read/grep.
		var sb strings.Builder
		for _, e := range entries {
			sb.WriteString(e.path)
			sb.WriteByte('\n')
		}
		outputPath = tool.WriteTruncFile("glob", sb.String())
		displayed = entries[:globLimit]
		for _, e := range displayed {
			lines = append(lines, e.path)
		}
		lines = append(lines, "")
		lines = append(lines, tool.BuildTruncHint(outputPath, len(entries), sb.Len()))
	} else {
		for _, e := range entries {
			lines = append(lines, e.path)
		}
	}

	return tool.Result{
		Output:     strings.Join(lines, "\n"),
		Truncated:  truncated,
		OutputPath: outputPath,
		Metadata: map[string]any{
			"count":     len(displayed),
			"truncated": truncated,
		},
	}, nil
}

// runRgGlob uses ripgrep's file-listing mode to match glob patterns.
// If maxResults <= 0, all matching paths are collected.
func runRgGlob(ctx context.Context, rgPath, dir, pattern string, maxResults int) ([]string, error) {
	cmd := exec.CommandContext(ctx, rgPath, "--files", "--glob", pattern, dir)
	out, err := cmd.Output()
	if err != nil {
		// Exit code 1 means no matches — not a real error.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			paths = append(paths, line)
			if maxResults > 0 && len(paths) >= maxResults {
				break
			}
		}
	}
	return paths, nil
}

// walkGlob is a pure-Go fallback when ripgrep is not available.
// If maxResults <= 0, all matching paths are collected.
func walkGlob(root, pattern string, maxResults int) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err2 := filepath.Rel(root, path)
		if err2 != nil {
			return nil
		}
		matched, err3 := filepath.Match(pattern, rel)
		if err3 != nil {
			return err3
		}
		if !matched {
			matched, _ = filepath.Match(pattern, filepath.Base(path))
		}
		if matched {
			paths = append(paths, path)
			if maxResults > 0 && len(paths) >= maxResults {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return paths, err
}
