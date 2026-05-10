package builtin

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/larryhou/llm-go/tool"
)

const (
	readDefaultLimit  = 2000      // lines
	readMaxBytes      = 50 * 1024 // 50 KB hard cap per call
	readMaxLineLen    = 2000      // chars per line before truncation
	readMaxBytesLabel = "50 KB"
)

var readMaxLineSuffix = fmt.Sprintf("... (line truncated to %d chars)", readMaxLineLen)

// ReadTool reads file contents (text, images, PDFs) or lists directory entries.
// Aligned with packages/opencode/src/tool/read.ts.
type ReadTool struct {
	WorkDir string
}

func (t *ReadTool) Name() string { return "read" }

func (t *ReadTool) Description() string {
	return `Read a file or directory from the local filesystem.
- Returns file contents with line numbers prefixed as "<n>: <content>".
- Supports text files, images (jpeg/png/gif/webp), and PDFs — images and PDFs are returned as binary attachments.
- Use offset and limit for pagination of large files (default limit: 2000 lines).
- For directories, lists entries one per line with trailing "/" for subdirectories.`
}

func (t *ReadTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filePath": map[string]any{
				"type":        "string",
				"description": "The absolute path to the file or directory to read.",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "1-indexed line number to start reading from. Defaults to 1.",
				"minimum":     1,
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Maximum number of lines to return. Defaults to %d.", readDefaultLimit),
				"minimum":     1,
			},
		},
		"required": []string{"filePath"},
	}
}

func (t *ReadTool) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
	filePath, _ := input["filePath"].(string)
	if filePath == "" {
		return tool.Result{}, tool.Fail("filePath is required")
	}
	if !filepath.IsAbs(filePath) {
		base := t.WorkDir
		if base == "" {
			base, _ = os.Getwd()
		}
		filePath = filepath.Join(base, filePath)
	}

	offset := 1
	if v, ok := input["offset"].(float64); ok && v >= 1 {
		offset = int(v)
	}
	limit := readDefaultLimit
	if v, ok := input["limit"].(float64); ok && v >= 1 {
		limit = int(v)
	}

	fi, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			suggestion := suggestSimilar(filePath)
			msg := fmt.Sprintf("File not found: %s", filePath)
			if suggestion != "" {
				msg += "\n" + suggestion
			}
			return tool.Result{}, tool.Fail(msg)
		}
		return tool.Result{}, tool.Fail(err.Error())
	}

	if fi.IsDir() {
		return t.readDir(filePath, offset, limit)
	}
	return t.readFile(filePath, offset, limit)
}

func (t *ReadTool) readDir(dirPath string, offset, limit int) (tool.Result, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return tool.Result{}, tool.Fail(err.Error())
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		} else if e.Type()&os.ModeSymlink != 0 {
			// Resolve symlink to check if it points to a dir.
			resolved, err2 := os.Stat(filepath.Join(dirPath, e.Name()))
			if err2 == nil && resolved.IsDir() {
				name += "/"
			}
		}
		names = append(names, name)
	}

	total := len(names)
	start := offset - 1
	if start >= total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	sliced := names[start:end]
	truncated := start+len(sliced) < total

	var footer string
	if truncated {
		footer = fmt.Sprintf("\n(Showing %d of %d entries. Use 'offset' parameter to read beyond entry %d)",
			len(sliced), total, offset+len(sliced))
	} else {
		footer = fmt.Sprintf("\n(%d entries)", total)
	}

	output := strings.Join([]string{
		fmt.Sprintf("<path>%s</path>", dirPath),
		"<type>directory</type>",
		"<entries>",
		strings.Join(sliced, "\n") + footer,
		"</entries>",
	}, "\n")
	return tool.Result{Output: output}, nil
}

func (t *ReadTool) readFile(filePath string, offset, limit int) (tool.Result, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return tool.Result{}, tool.Fail(err.Error())
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return tool.Result{}, tool.Fail(err.Error())
	}

	// Sniff first 4096 bytes for MIME type / binary detection.
	sampleSize := 4096
	if fi.Size() < int64(sampleSize) {
		sampleSize = int(fi.Size())
	}
	sample := make([]byte, sampleSize)
	n, _ := f.Read(sample)
	sample = sample[:n]

	mimeType := http.DetectContentType(sample)

	// Check for image/PDF — return as attachment.
	switch {
	case strings.HasPrefix(mimeType, "image/jpeg"),
		strings.HasPrefix(mimeType, "image/png"),
		strings.HasPrefix(mimeType, "image/gif"),
		strings.HasPrefix(mimeType, "image/webp"):
		return t.readBinary(f, filePath, mimeType, sample)
	case isPDF(sample):
		return t.readBinary(f, filePath, "application/pdf", sample)
	}

	// Extension-based binary check (mirrors opencode isBinaryFile allowlist).
	if isBinaryExt(filePath) || isBinary(sample) {
		return tool.Result{}, tool.Fail(fmt.Sprintf("Cannot read binary file: %s", filePath))
	}

	// Seek back to start.
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return tool.Result{}, tool.Fail(err.Error())
	}

	// Stream lines — mirrors opencode's lines() async function logic.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	start := offset - 1
	var raw []string
	totalBytes := 0
	count := 0
	cut := false
	more := false

	for scanner.Scan() {
		count++
		if count <= start {
			continue
		}
		if len(raw) >= limit {
			more = true
			continue
		}
		line := scanner.Text()
		if len([]rune(line)) > readMaxLineLen {
			line = string([]rune(line)[:readMaxLineLen]) + readMaxLineSuffix
		}
		size := len(line)
		if len(raw) > 0 {
			size++ // newline separator
		}
		if totalBytes+size > readMaxBytes {
			cut = true
			more = true
			break
		}
		raw = append(raw, line)
		totalBytes += size
	}
	if err2 := scanner.Err(); err2 != nil {
		return tool.Result{}, tool.Fail(fmt.Sprintf("read error: %v", err2))
	}

	// Validate offset.
	if count < offset-1 && !(count == 0 && offset == 1) {
		return tool.Result{}, tool.Fail(fmt.Sprintf("Offset %d is out of range for this file (%d lines)", offset, count))
	}

	// Build numbered output.
	var sb strings.Builder
	for i, line := range raw {
		fmt.Fprintf(&sb, "%d: %s\n", i+offset, line)
	}

	last := offset + len(raw) - 1
	next := last + 1
	var footer string
	if cut {
		footer = fmt.Sprintf("\n(Output capped at %s. Showing lines %d-%d. Use offset=%d to continue.)", readMaxBytesLabel, offset, last, next)
	} else if more {
		footer = fmt.Sprintf("\n(Showing lines %d-%d of %d. Use offset=%d to continue.)", offset, last, count, next)
	} else {
		footer = fmt.Sprintf("\n(End of file - total %d lines)", count)
	}

	output := "<path>" + filePath + "</path>\n<type>file</type>\n<content>\n" +
		sb.String() + footer + "\n</content>"

	return tool.Result{Output: output}, nil
}

func (t *ReadTool) readBinary(f *os.File, filePath, mimeType string, sample []byte) (tool.Result, error) {
	rest, err := io.ReadAll(f)
	if err != nil {
		return tool.Result{}, tool.Fail(err.Error())
	}
	data := append(sample, rest...)
	return tool.Result{
		Output: fmt.Sprintf("<path>%s</path>\n<type>%s</type>", filePath, mimeType),
		Attachments: []tool.Attachment{
			{MediaType: mimeType, Data: data, Name: filepath.Base(filePath)},
		},
	}, nil
}

// isBinaryExt returns true for known binary file extensions.
// Mirrors opencode's isBinaryFile extension switch.
func isBinaryExt(filePath string) bool {
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".zip", ".tar", ".gz", ".exe", ".dll", ".so",
		".class", ".jar", ".war", ".7z",
		".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".odt", ".ods", ".odp",
		".bin", ".dat", ".obj", ".o", ".a", ".lib",
		".wasm", ".pyc", ".pyo":
		return true
	}
	return false
}
func isBinary(sample []byte) bool {
	if len(sample) == 0 {
		return false
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	nonPrintable := 0
	for _, b := range sample {
		if b < 0x09 || (b > 0x0d && b < 0x20) || b == 0x7f {
			nonPrintable++
		}
	}
	return float64(nonPrintable)/float64(len(sample)) > 0.30
}

// isPDF checks the PDF magic bytes.
func isPDF(sample []byte) bool {
	return bytes.HasPrefix(sample, []byte("%PDF"))
}

// suggestSimilar looks for case-insensitively similar filenames in the same directory.
func suggestSimilar(filePath string) string {
	dir := filepath.Dir(filePath)
	base := strings.ToLower(filepath.Base(filePath))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var suggestions []string
	for _, e := range entries {
		if strings.ToLower(e.Name()) == base {
			suggestions = append(suggestions, filepath.Join(dir, e.Name()))
		}
		if len(suggestions) >= 3 {
			break
		}
	}
	if len(suggestions) == 0 {
		return ""
	}
	return "Did you mean:\n  " + strings.Join(suggestions, "\n  ")
}
