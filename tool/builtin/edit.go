package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/larryhou/llm-go/tool"
)

// EditTool applies surgical string-replacement edits to an existing file.
// Aligned with packages/opencode/src/tool/edit.ts.
type EditTool struct {
	WorkDir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (t *EditTool) Name() string { return "edit" }

func (t *EditTool) Description() string {
	return `Perform exact string replacements in files.
- You must read the file at least once before editing to know the exact content.
- oldString must match the file content exactly (including whitespace and indentation).
- If oldString appears multiple times, provide more surrounding context to make it unique.
- Set replaceAll to true to rename a variable or replace every occurrence.
- If oldString is empty the file is created/overwritten with newString (same as write tool).`
}

func (t *EditTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filePath": map[string]any{
				"type":        "string",
				"description": "The absolute path to the file to modify.",
			},
			"oldString": map[string]any{
				"type":        "string",
				"description": "The text to find and replace. Must match exactly.",
			},
			"newString": map[string]any{
				"type":        "string",
				"description": "The replacement text.",
			},
			"replaceAll": map[string]any{
				"type":        "boolean",
				"description": "Replace all occurrences of oldString. Default false.",
			},
		},
		"required": []string{"filePath", "oldString", "newString"},
	}
}

func (t *EditTool) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
	filePath, _ := input["filePath"].(string)
	if filePath == "" {
		return tool.Result{}, tool.Fail("filePath is required")
	}
	oldString, _ := input["oldString"].(string)
	newString, _ := input["newString"].(string)
	replaceAll, _ := input["replaceAll"].(bool)

	if !filepath.IsAbs(filePath) {
		base := t.WorkDir
		if base == "" {
			base, _ = os.Getwd()
		}
		filePath = filepath.Join(base, filePath)
	}

	if oldString == newString {
		return tool.Result{}, tool.Fail("No changes to apply: oldString and newString are identical.")
	}

	// File creation mode when oldString is empty.
	if oldString == "" {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return tool.Result{}, tool.Fail(fmt.Sprintf("failed to create directories: %v", err))
		}
		if err := os.WriteFile(filePath, []byte(newString), 0o644); err != nil {
			return tool.Result{}, tool.Fail(fmt.Sprintf("failed to write file: %v", err))
		}
		return tool.Result{Output: "Edit applied successfully."}, nil
	}

	// Per-file mutex to prevent concurrent edits.
	fileMu := t.fileMutex(filePath)
	fileMu.Lock()
	defer fileMu.Unlock()

	raw, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return tool.Result{}, tool.Fail(fmt.Sprintf("file not found: %s", filePath))
		}
		return tool.Result{}, tool.Fail(err.Error())
	}

	// Detect and preserve line endings.
	content := string(raw)
	ending := detectLineEnding(content)
	// Normalise both content and search strings to LF, then convert to detected ending.
	normalised := normalizeLineEndings(content)
	oldNorm := convertLineEnding(normalizeLineEndings(oldString), ending)
	newNorm := convertLineEnding(normalizeLineEndings(newString), ending)

	result, err2 := editReplace(content, oldNorm, newNorm, replaceAll)
	if err2 != nil {
		return tool.Result{}, tool.Fail(err2.Error())
	}

	if err := os.WriteFile(filePath, []byte(result), 0o644); err != nil {
		return tool.Result{}, tool.Fail(fmt.Sprintf("failed to write file: %v", err))
	}

	_ = normalised // used implicitly via normalizeLineEndings above
	added, removed := diffStats(content, result)
	return tool.Result{
		Output: "Edit applied successfully.",
		Metadata: map[string]any{
			"path":      filePath,
			"additions": added,
			"deletions": removed,
		},
	}, nil
}

func (t *EditTool) fileMutex(path string) *sync.Mutex {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.locks == nil {
		t.locks = make(map[string]*sync.Mutex)
	}
	if mu, ok := t.locks[path]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	t.locks[path] = mu
	return mu
}

func normalizeLineEndings(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func detectLineEnding(s string) string {
	if strings.Contains(s, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

func convertLineEnding(s, ending string) string {
	if ending == "\n" {
		return s
	}
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// Replacer is a generator that yields candidate substrings of content matching find.
type replacerFunc func(content, find string) []string

// editReplace mirrors opencode's replace() function in edit.ts.
// Iterates through all 9 replacer strategies in order.
func editReplace(content, oldString, newString string, replaceAll bool) (string, error) {
	notFound := true

	for _, replacer := range []replacerFunc{
		simpleReplacer,
		lineTrimmedReplacer,
		blockAnchorReplacer,
		whitespaceNormalizedReplacer,
		indentationFlexibleReplacer,
		escapeNormalizedReplacer,
		trimmedBoundaryReplacer,
		contextAwareReplacer,
		multiOccurrenceReplacer,
	} {
		for _, search := range replacer(content, oldString) {
			idx := strings.Index(content, search)
			if idx == -1 {
				continue
			}
			notFound = false
			if replaceAll {
				return strings.ReplaceAll(content, search, newString), nil
			}
			lastIdx := strings.LastIndex(content, search)
			if idx != lastIdx {
				continue
			}
			return content[:idx] + newString + content[idx+len(search):], nil
		}
	}

	if notFound {
		return "", fmt.Errorf("Could not find oldString in the file. It must match exactly, including whitespace, indentation, and line endings.")
	}
	return "", fmt.Errorf("Found multiple matches for oldString. Provide more surrounding lines in oldString to identify the correct match.")
}

// simpleReplacer yields the find string itself (exact match).
func simpleReplacer(_, find string) []string {
	return []string{find}
}

// lineTrimmedReplacer matches by trimming each line's whitespace independently.
// Mirrors opencode's LineTrimmedReplacer.
func lineTrimmedReplacer(content, find string) []string {
	originalLines := strings.Split(content, "\n")
	searchLines := strings.Split(find, "\n")
	// Remove trailing empty line if present.
	if len(searchLines) > 0 && searchLines[len(searchLines)-1] == "" {
		searchLines = searchLines[:len(searchLines)-1]
	}

	var results []string
	for i := 0; i <= len(originalLines)-len(searchLines); i++ {
		match := true
		for j := range searchLines {
			if strings.TrimSpace(originalLines[i+j]) != strings.TrimSpace(searchLines[j]) {
				match = false
				break
			}
		}
		if match {
			// Compute byte offsets for this match in content.
			startIdx := 0
			for k := 0; k < i; k++ {
				startIdx += len(originalLines[k]) + 1
			}
			endIdx := startIdx
			for k := 0; k < len(searchLines); k++ {
				endIdx += len(originalLines[i+k])
				if k < len(searchLines)-1 {
					endIdx++ // newline
				}
			}
			results = append(results, content[startIdx:endIdx])
		}
	}
	return results
}

// blockAnchorReplacer matches using first/last line as anchors with Levenshtein similarity.
// Mirrors opencode's BlockAnchorReplacer.
func blockAnchorReplacer(content, find string) []string {
	const singleThreshold = 0.0
	const multiThreshold = 0.3

	originalLines := strings.Split(content, "\n")
	searchLines := strings.Split(find, "\n")
	if len(searchLines) < 3 {
		return nil
	}
	if searchLines[len(searchLines)-1] == "" {
		searchLines = searchLines[:len(searchLines)-1]
	}

	firstLine := strings.TrimSpace(searchLines[0])
	lastLine := strings.TrimSpace(searchLines[len(searchLines)-1])
	searchBlockSize := len(searchLines)

	type candidate struct{ startLine, endLine int }
	var candidates []candidate
	for i := 0; i < len(originalLines); i++ {
		if strings.TrimSpace(originalLines[i]) != firstLine {
			continue
		}
		for j := i + 2; j < len(originalLines); j++ {
			if strings.TrimSpace(originalLines[j]) == lastLine {
				candidates = append(candidates, candidate{i, j})
				break
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	similarity := func(c candidate) float64 {
		actualSize := c.endLine - c.startLine + 1
		linesToCheck := min2(searchBlockSize-2, actualSize-2)
		if linesToCheck <= 0 {
			return 1.0
		}
		var sim float64
		for j := 1; j < searchBlockSize-1 && j < actualSize-1; j++ {
			ol := strings.TrimSpace(originalLines[c.startLine+j])
			sl := strings.TrimSpace(searchLines[j])
			maxLen := max2(len(ol), len(sl))
			if maxLen == 0 {
				continue
			}
			d := levenshtein(ol, sl)
			sim += (1 - float64(d)/float64(maxLen)) / float64(linesToCheck)
			if len(candidates) == 1 && sim >= singleThreshold {
				break
			}
		}
		return sim
	}

	extractMatch := func(c candidate) string {
		startIdx := 0
		for k := 0; k < c.startLine; k++ {
			startIdx += len(originalLines[k]) + 1
		}
		endIdx := startIdx
		for k := c.startLine; k <= c.endLine; k++ {
			endIdx += len(originalLines[k])
			if k < c.endLine {
				endIdx++
			}
		}
		return content[startIdx:endIdx]
	}

	if len(candidates) == 1 {
		if similarity(candidates[0]) >= singleThreshold {
			return []string{extractMatch(candidates[0])}
		}
		return nil
	}

	var best candidate
	maxSim := -1.0
	for _, c := range candidates {
		if s := similarity(c); s > maxSim {
			maxSim = s
			best = c
		}
	}
	if maxSim >= multiThreshold {
		return []string{extractMatch(best)}
	}
	return nil
}

// whitespaceNormalizedReplacer collapses whitespace before matching.
// Mirrors opencode's WhitespaceNormalizedReplacer.
func whitespaceNormalizedReplacer(content, find string) []string {
	normalizeWS := func(s string) string {
		return strings.Join(strings.Fields(s), " ")
	}
	normalizedFind := normalizeWS(find)
	contentLines := strings.Split(content, "\n")
	findLines := strings.Split(find, "\n")

	var results []string
	// Single-line check.
	for _, line := range contentLines {
		if normalizeWS(line) == normalizedFind {
			results = append(results, line)
			continue
		}
		// Substring match: find the actual word-sequence in the original line.
		// Only yield when find is multi-word (mirrors TS regex approach).
		words := strings.Fields(strings.TrimSpace(find))
		if len(words) > 1 && strings.Contains(normalizeWS(line), normalizedFind) {
			// Scan for the contiguous sequence of words in the line.
			lineWords := strings.Fields(line)
			for i := 0; i <= len(lineWords)-len(words); i++ {
				if strings.Join(lineWords[i:i+len(words)], " ") == normalizedFind {
					results = append(results, strings.Join(lineWords[i:i+len(words)], " "))
					break
				}
			}
		}
	}
	// Multi-line check.
	if len(findLines) > 1 {
		for i := 0; i <= len(contentLines)-len(findLines); i++ {
			block := strings.Join(contentLines[i:i+len(findLines)], "\n")
			if normalizeWS(block) == normalizedFind {
				results = append(results, block)
			}
		}
	}
	return results
}

// indentationFlexibleReplacer strips common leading indentation.
// Mirrors opencode's IndentationFlexibleReplacer.
func indentationFlexibleReplacer(content, find string) []string {
	removeIndent := func(text string) string {
		lines := strings.Split(text, "\n")
		nonEmpty := make([]string, 0, len(lines))
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				nonEmpty = append(nonEmpty, l)
			}
		}
		if len(nonEmpty) == 0 {
			return text
		}
		minIndent := -1
		for _, l := range nonEmpty {
			indent := len(l) - len(strings.TrimLeft(l, " \t"))
			if minIndent < 0 || indent < minIndent {
				minIndent = indent
			}
		}
		var out []string
		for _, l := range lines {
			if strings.TrimSpace(l) == "" {
				out = append(out, l)
			} else {
				out = append(out, l[minIndent:])
			}
		}
		return strings.Join(out, "\n")
	}

	normalizedFind := removeIndent(find)
	contentLines := strings.Split(content, "\n")
	findLines := strings.Split(find, "\n")

	var results []string
	for i := 0; i <= len(contentLines)-len(findLines); i++ {
		block := strings.Join(contentLines[i:i+len(findLines)], "\n")
		if removeIndent(block) == normalizedFind {
			results = append(results, block)
		}
	}
	return results
}

// escapeNormalizedReplacer unescapes escape sequences before matching.
// Mirrors opencode's EscapeNormalizedReplacer.
func escapeNormalizedReplacer(content, find string) []string {
	unescape := func(s string) string {
		var sb strings.Builder
		i := 0
		runes := []rune(s)
		for i < len(runes) {
			if runes[i] == '\\' && i+1 < len(runes) {
				switch runes[i+1] {
				case 'n':
					sb.WriteByte('\n')
				case 't':
					sb.WriteByte('\t')
				case 'r':
					sb.WriteByte('\r')
				case '\'':
					sb.WriteByte('\'')
				case '"':
					sb.WriteByte('"')
				case '`':
					sb.WriteByte('`')
				case '\\':
					sb.WriteByte('\\')
				case '$':
					sb.WriteByte('$')
				default:
					sb.WriteRune(runes[i])
					sb.WriteRune(runes[i+1])
				}
				i += 2
				continue
			}
			sb.WriteRune(runes[i])
			i++
		}
		return sb.String()
	}

	unescapedFind := unescape(find)
	var results []string
	if strings.Contains(content, unescapedFind) {
		results = append(results, unescapedFind)
	}
	// Also check unescaped blocks in content.
	contentLines := strings.Split(content, "\n")
	findLines := strings.Split(unescapedFind, "\n")
	for i := 0; i <= len(contentLines)-len(findLines); i++ {
		block := strings.Join(contentLines[i:i+len(findLines)], "\n")
		if unescape(block) == unescapedFind {
			results = append(results, block)
		}
	}
	return results
}

// trimmedBoundaryReplacer trims leading/trailing whitespace from find.
// Mirrors opencode's TrimmedBoundaryReplacer.
func trimmedBoundaryReplacer(content, find string) []string {
	trimmed := strings.TrimSpace(find)
	if trimmed == find {
		return nil
	}
	var results []string
	if strings.Contains(content, trimmed) {
		results = append(results, trimmed)
	}
	contentLines := strings.Split(content, "\n")
	findLines := strings.Split(find, "\n")
	for i := 0; i <= len(contentLines)-len(findLines); i++ {
		block := strings.Join(contentLines[i:i+len(findLines)], "\n")
		if strings.TrimSpace(block) == trimmed {
			results = append(results, block)
		}
	}
	return results
}

// contextAwareReplacer uses first/last line as context anchors with ≥50% middle match.
// Mirrors opencode's ContextAwareReplacer.
func contextAwareReplacer(content, find string) []string {
	findLines := strings.Split(find, "\n")
	if len(findLines) < 3 {
		return nil
	}
	if findLines[len(findLines)-1] == "" {
		findLines = findLines[:len(findLines)-1]
	}
	contentLines := strings.Split(content, "\n")
	firstLine := strings.TrimSpace(findLines[0])
	lastLine := strings.TrimSpace(findLines[len(findLines)-1])

	var results []string
	for i := 0; i < len(contentLines); i++ {
		if strings.TrimSpace(contentLines[i]) != firstLine {
			continue
		}
		for j := i + 2; j < len(contentLines); j++ {
			if strings.TrimSpace(contentLines[j]) != lastLine {
				continue
			}
			blockLines := contentLines[i : j+1]
			if len(blockLines) != len(findLines) {
				break
			}
			matchingLines := 0
			totalNonEmpty := 0
			for k := 1; k < len(blockLines)-1; k++ {
				bl := strings.TrimSpace(blockLines[k])
				fl := strings.TrimSpace(findLines[k])
				if bl != "" || fl != "" {
					totalNonEmpty++
					if bl == fl {
						matchingLines++
					}
				}
			}
			if totalNonEmpty == 0 || float64(matchingLines)/float64(totalNonEmpty) >= 0.5 {
				startIdx := 0
				for k := 0; k < i; k++ {
					startIdx += len(contentLines[k]) + 1
				}
				endIdx := startIdx
				for k := i; k <= j; k++ {
					endIdx += len(contentLines[k])
					if k < j {
						endIdx++
					}
				}
				results = append(results, content[startIdx:endIdx])
			}
			break
		}
	}
	return results
}

// multiOccurrenceReplacer yields all exact matches.
// Mirrors opencode's MultiOccurrenceReplacer.
func multiOccurrenceReplacer(content, find string) []string {
	var results []string
	start := 0
	for {
		idx := strings.Index(content[start:], find)
		if idx == -1 {
			break
		}
		results = append(results, find)
		start += idx + len(find)
	}
	return results
}

// levenshtein computes the edit distance between two strings.
func levenshtein(a, b string) int {
	if a == "" || b == "" {
		if len(a) > len(b) {
			return len(a)
		}
		return len(b)
	}
	ra, rb := []rune(a), []rune(b)
	matrix := make([][]int, len(ra)+1)
	for i := range matrix {
		matrix[i] = make([]int, len(rb)+1)
		matrix[i][0] = i
	}
	for j := range matrix[0] {
		matrix[0][j] = j
	}
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			matrix[i][j] = min2(matrix[i-1][j]+1, min2(matrix[i][j-1]+1, matrix[i-1][j-1]+cost))
		}
	}
	return matrix[len(ra)][len(rb)]
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// diffStats counts added and removed lines between old and new content.
func diffStats(oldContent, newContent string) (added, removed int) {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")
	if len(newLines) > len(oldLines) {
		added = len(newLines) - len(oldLines)
	} else {
		removed = len(oldLines) - len(newLines)
	}
	return
}
