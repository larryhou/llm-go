package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Truncate constants, aligned with packages/opencode/src/tool/truncate.ts.
const (
	DefaultMaxLines = 2000
	DefaultMaxBytes = 50 * 1024 // 51,200 bytes
	TruncationDir   = "" // set at init from os.TempDir()
	RetentionDays   = 7
)

// TruncateOptions configures truncation behaviour.
type TruncateOptions struct {
	MaxLines  int
	MaxBytes  int
	Direction string // "head" (default) or "tail"
}

// TruncateResult holds the (possibly truncated) content.
type TruncateResult struct {
	Content   string
	Truncated bool
	// OutputPath is the path where the full content was written when truncated.
	OutputPath string
}

var truncDir string

func init() {
	truncDir = filepath.Join(os.TempDir(), "opencode-tool-output")
	// Background cleanup: remove files older than 7 days
	go cleanupLoop()
}

// Truncate applies line/byte limits to text.
// Aligned with packages/opencode/src/tool/truncate.ts output().
func Truncate(toolName, text string, opts *TruncateOptions) TruncateResult {
	maxLines := DefaultMaxLines
	maxBytes := DefaultMaxBytes
	direction := "head"
	if opts != nil {
		if opts.MaxLines > 0 {
			maxLines = opts.MaxLines
		}
		if opts.MaxBytes > 0 {
			maxBytes = opts.MaxBytes
		}
		if opts.Direction != "" {
			direction = opts.Direction
		}
	}

	lines := strings.Split(text, "\n")
	byteLen := len(text)

	if len(lines) <= maxLines && byteLen <= maxBytes {
		return TruncateResult{Content: text, Truncated: false}
	}

	// Build preview respecting direction
	var preview []string
	previewBytes := 0
	if direction == "head" {
		for _, line := range lines {
			lineBytes := utf8.RuneCountInString(line) + 1 // +1 for \n
			if len(preview) >= maxLines || previewBytes+lineBytes > maxBytes {
				break
			}
			preview = append(preview, line)
			previewBytes += lineBytes
		}
	} else { // tail
		for i := len(lines) - 1; i >= 0; i-- {
			line := lines[i]
			lineBytes := utf8.RuneCountInString(line) + 1
			if len(preview) >= maxLines || previewBytes+lineBytes > maxBytes {
				break
			}
			preview = append([]string{line}, preview...)
			previewBytes += lineBytes
		}
	}

	// Write full content to temp file
	outputPath := writeTruncationFile(toolName, text)

	removedLines := len(lines) - len(preview)
	removedBytes := byteLen - previewBytes

	hint := buildHint(outputPath)
	var content string
	if direction == "head" {
		content = strings.Join(preview, "\n")
		if removedBytes > 0 {
			content += fmt.Sprintf("\n\n...%d lines / %d bytes truncated...\n\n%s", removedLines, removedBytes, hint)
		}
	} else {
		content = fmt.Sprintf("...%d lines / %d bytes truncated...\n\n%s\n\n", removedLines, removedBytes, hint)
		content += strings.Join(preview, "\n")
	}

	return TruncateResult{
		Content:    content,
		Truncated:  true,
		OutputPath: outputPath,
	}
}

func buildHint(outputPath string) string {
	if outputPath == "" {
		return "Output was truncated. Use Read with offset/limit or Grep to access specific sections."
	}
	return fmt.Sprintf("Full output saved to: %s\nUse Read (with offset/limit) or Grep to search the full content.", outputPath)
}

func writeTruncationFile(toolName, content string) string {
	if err := os.MkdirAll(truncDir, 0o750); err != nil {
		return ""
	}
	name := fmt.Sprintf("%s-%d.txt", toolName, time.Now().UnixNano())
	path := filepath.Join(truncDir, name)
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		return ""
	}
	return path
}

func cleanupLoop() {
	// Initial delay of 1 minute before first cleanup
	time.Sleep(time.Minute)
	for {
		cleanupOldFiles()
		time.Sleep(time.Hour)
	}
}

func cleanupOldFiles() {
	entries, err := os.ReadDir(truncDir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -RetentionDays)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(truncDir, e.Name()))
		}
	}
}
