package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Truncate constants, aligned with packages/opencode/src/tool/truncate.ts.
const (
	DefaultMaxLines = 2000
	DefaultMaxBytes = 50 * 1024 // 51,200 bytes
	TruncationDir   = "" // placeholder; actual temp dir is set in truncDir via init()
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
}

// StartCleanup starts the background cleanup goroutine that removes truncation
// files older than RetentionDays. It respects ctx cancellation so it exits
// cleanly when the process shuts down. Call once from main().
func StartCleanup(ctx context.Context) {
	go func() {
		// Initial delay of 1 minute before first cleanup
		select {
		case <-time.After(time.Minute):
		case <-ctx.Done():
			return
		}
		for {
			cleanupOldFiles()
			select {
			case <-time.After(time.Hour):
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Truncate applies line/byte limits to text.
// Aligned with packages/opencode/src/tool/truncate.ts output().
func Truncate(toolName, text string, opts *TruncateOptions) TruncateResult {
	maxLines := DefaultMaxLines
	maxBytes := DefaultMaxBytes
	if opts != nil {
		if opts.MaxLines > 0 {
			maxLines = opts.MaxLines
		}
		if opts.MaxBytes > 0 {
			maxBytes = opts.MaxBytes
		}
	}

	lines := strings.Split(text, "\n")
	byteLen := len(text)

	if len(lines) <= maxLines && byteLen <= maxBytes {
		return TruncateResult{Content: text, Truncated: false}
	}

	// Write full content to temp file; return only the path hint — no preview.
	outputPath := writeTruncationFile(toolName, text)

	content := buildHint(outputPath, len(lines), byteLen)

	return TruncateResult{
		Content:    content,
		Truncated:  true,
		OutputPath: outputPath,
	}
}

func buildHint(outputPath string, lines, bytes int) string {
	return BuildTruncHint(outputPath, lines, bytes)
}

// BuildTruncHint returns the message shown to the LLM when tool output is too
// large. It includes the line count, raw byte count, actual file size on disk,
// and the full path so the LLM can explore the content with read/grep tools.
// Exported so sub-packages (e.g. tool/builtin) can reuse the same format.
func BuildTruncHint(outputPath string, lines, bytes int) string {
	size := fmt.Sprintf("%d lines / %d bytes", lines, bytes)
	if outputPath == "" {
		return fmt.Sprintf("Output is too large (%s) and could not be saved to a file. Use more targeted parameters to reduce the output size.", size)
	}
	// Report the actual file size so the LLM knows what it is dealing with.
	fileSize := ""
	if fi, err := os.Stat(outputPath); err == nil {
		fileSize = fmt.Sprintf(", file size: %s", formatBytes(fi.Size()))
	}
	return fmt.Sprintf(
		"Output is too large (%s%s). Full content written to: %s\nUse the read tool (with offset/limit) or grep tool to explore the content.",
		size, fileSize, outputPath,
	)
}

// formatBytes returns a human-readable byte size string (e.g. "1.2 MB").
func formatBytes(n int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1f GB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// WriteTruncFile writes content to a temp truncation file and returns the path.
// Exported for use by sub-packages (e.g. tool/builtin).
func WriteTruncFile(toolName, content string) string {
	return writeTruncationFile(toolName, content)
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
