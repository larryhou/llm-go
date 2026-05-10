package builtin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/larryhou/llm-go/tool"
)

const (
	shellDefaultTimeout = 120 * time.Second
	shellMaxBytes       = 100 * 1024 // 100 KB tail buffer
	shellMaxLines       = 2000
)

// ShellTool executes shell commands (bash on Unix/macOS, cmd/powershell on Windows).
// Aligned with packages/opencode/src/tool/shell.ts.
// Tool name is "bash" for API/plugin compatibility.
type ShellTool struct {
	// WorkDir is the default working directory. Defaults to os.Getwd().
	WorkDir string
	// Shell overrides the shell binary (e.g. "/bin/bash"). Auto-detected if empty.
	Shell string
}

func (t *ShellTool) Name() string { return "bash" }

func (t *ShellTool) Description() string {
	if runtime.GOOS == "windows" {
		return `Executes a given command in a persistent shell session (cmd.exe / PowerShell).
- Use && to chain sequential commands.
- Use the workdir parameter instead of cd.
- Avoid interactive commands; stdin is always closed.
- Specify a timeout (in milliseconds) for long-running operations.`
	}
	return `Executes a given bash command in a persistent shell session with optional timeout.
- Use && to chain sequential commands that depend on each other.
- Use the workdir parameter instead of cd commands.
- Avoid interactive commands; stdin is always closed.
- Do not use find, grep, cat, head, tail, sed, awk when dedicated tools are available.
- Quote file paths that contain spaces with double quotes.`
}

func (t *ShellTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "Clear, concise description of what this command does in 5-10 words.",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Optional timeout in milliseconds. Defaults to %d ms (%s).", shellDefaultTimeout.Milliseconds(), shellDefaultTimeout),
				"minimum":     1,
			},
			"workdir": map[string]any{
				"type":        "string",
				"description": "Working directory for the command. Defaults to the project root. Use this instead of cd commands.",
			},
		},
		"required": []string{"command", "description"},
	}
}

func (t *ShellTool) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
	command, _ := input["command"].(string)
	if command == "" {
		return tool.Result{}, tool.Fail("command is required")
	}

	timeout := shellDefaultTimeout
	if v, ok := input["timeout"].(float64); ok && v < 0 {
		return tool.Result{}, tool.Fail(fmt.Sprintf("Invalid timeout value: %v. Timeout must be a positive number.", v))
	}
	if v, ok := input["timeout"].(float64); ok && v > 0 {
		timeout = time.Duration(v) * time.Millisecond
	}

	workdir := t.WorkDir
	if wd, ok := input["workdir"].(string); ok && wd != "" {
		if filepath.IsAbs(wd) {
			workdir = wd
		} else {
			base := t.WorkDir
			if base == "" {
				base, _ = os.Getwd()
			}
			workdir = filepath.Join(base, wd)
		}
	}
	if workdir == "" {
		workdir, _ = os.Getwd()
	}

	shell, args := t.resolveShell(command)

	// Create a context with timeout.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, shell, args...)
	cmd.Dir = workdir
	cmd.Stdin = nil // non-interactive

	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()
	rawOutput := buf.String()

	// Determine exit code.
	exitCode := 0
	timedOut := false
	aborted := false

	if runErr != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			timedOut = true
		} else if ctx.Err() != nil {
			aborted = true
		} else if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	// Truncate output (tail-biased like opencode's shell.ts tail()).
	tailResult := tailOutput(rawOutput, shellMaxLines, shellMaxBytes)
	output := tailResult.text
	truncated := tailResult.cut

	if output == "" {
		output = "(no output)"
	}

	// Append metadata block — mirrors opencode's <shell_metadata> block.
	var meta []string
	if timedOut {
		meta = append(meta, fmt.Sprintf(
			"shell tool terminated command after exceeding timeout %d ms. If this command is expected to take longer and is not waiting for interactive input, retry with a larger timeout value in milliseconds.",
			timeout.Milliseconds(),
		))
	}
	if aborted {
		meta = append(meta, "User aborted the command")
	}
	if len(meta) > 0 {
		output += "\n\n<shell_metadata>\n" + strings.Join(meta, "\n") + "\n</shell_metadata>"
	}

	var outputPath string
	if truncated && rawOutput != "" {
		outputPath = writeTruncationFile("bash", rawOutput)
		output = "...output truncated...\n\nFull output saved to: " + outputPath + "\n\n" + output
	}

	return tool.Result{
		Output:     output,
		Truncated:  truncated,
		OutputPath: outputPath,
		Metadata: map[string]any{
			"exit":        exitCode,
			"timed_out":   timedOut,
			"aborted":     aborted,
			"truncated":   truncated,
		},
	}, nil
}

// resolveShell returns the shell binary and arguments for the current platform.
func (t *ShellTool) resolveShell(command string) (string, []string) {
	if t.Shell != "" {
		return t.Shell, []string{"-c", command}
	}
	if runtime.GOOS == "windows" {
		// Prefer PowerShell 7+ if available, then cmd.
		if pwsh, err := exec.LookPath("pwsh"); err == nil {
			return pwsh, []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command}
		}
		return "cmd.exe", []string{"/C", command}
	}
	// Unix: prefer bash, fallback to sh.
	if bash, err := exec.LookPath("bash"); err == nil {
		return bash, []string{"-c", command}
	}
	return "/bin/sh", []string{"-c", command}
}

type tailResult struct {
	text string
	cut  bool
}

// tailOutput mirrors opencode's tail() function in shell.ts:
// iterates from the end accumulating lines within byte/line limits.
func tailOutput(s string, maxLines, maxBytes int) tailResult {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines && len(s) <= maxBytes {
		return tailResult{text: s, cut: false}
	}

	var out []string
	totalBytes := 0
	for i := len(lines) - 1; i >= 0 && len(out) < maxLines; i-- {
		lineBytes := len(lines[i])
		if len(out) > 0 {
			lineBytes++ // newline separator
		}
		if totalBytes+lineBytes > maxBytes {
			if len(out) == 0 {
				// Single line too large: trim from start, preserve valid UTF-8.
				b := []byte(lines[i])
				start := len(b) - maxBytes
				if start < 0 {
					start = 0
				}
				for start < len(b) && (b[start]&0xc0) == 0x80 {
					start++
				}
				out = append([]string{string(b[start:])}, out...)
			}
			break
		}
		out = append([]string{lines[i]}, out...)
		totalBytes += lineBytes
	}
	return tailResult{text: strings.Join(out, "\n"), cut: true}
}

// writeTruncationFile delegates to the shared helper in truncate.go (same package).
func writeTruncationFile(toolName, content string) string {
	return tool.WriteTruncFile(toolName, content)
}

func init() {
	_ = os.MkdirAll(filepath.Join(os.TempDir(), "opencode-tool-output"), 0o750)
}
