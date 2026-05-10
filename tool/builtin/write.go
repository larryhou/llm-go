package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/larryhou/llm-go/tool"
)

// WriteTool creates or overwrites a file with the provided content.
// Aligned with packages/opencode/src/tool/write.ts.
type WriteTool struct {
	WorkDir string
}

func (t *WriteTool) Name() string { return "write" }

func (t *WriteTool) Description() string {
	return `Write a file to the local filesystem, creating or overwriting it.
- The filePath must be an absolute path.
- Intermediate directories are created automatically.
- Prefer editing existing files with the edit tool when only partial changes are needed.
- Only create new files when explicitly required.`
}

func (t *WriteTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filePath": map[string]any{
				"type":        "string",
				"description": "The absolute path to the file to write.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The full content to write to the file.",
			},
		},
		"required": []string{"filePath", "content"},
	}
}

func (t *WriteTool) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
	filePath, _ := input["filePath"].(string)
	if filePath == "" {
		return tool.Result{}, tool.Fail("filePath is required")
	}
	content, _ := input["content"].(string)

	if !filepath.IsAbs(filePath) {
		base := t.WorkDir
		if base == "" {
			base, _ = os.Getwd()
		}
		filePath = filepath.Join(base, filePath)
	}

	// Ensure parent directories exist.
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return tool.Result{}, tool.Fail(fmt.Sprintf("failed to create directories: %v", err))
	}

	existed := false
	if _, err := os.Stat(filePath); err == nil {
		existed = true
	}

	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		return tool.Result{}, tool.Fail(fmt.Sprintf("failed to write file: %v", err))
	}

	return tool.Result{
		Output: "Wrote file successfully.",
		Metadata: map[string]any{
			"filepath": filePath,
			"exists":   existed,
		},
	}, nil
}
