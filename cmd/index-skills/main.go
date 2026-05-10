// Command index-skills scans a directory of skill Markdown files and builds
// (or rebuilds) a Bleve full-text search index that can be used by the
// knowledge manager at runtime.
//
// Usage:
//
//	go run ./cmd/index-skills \
//	    -skills /path/to/.opencode/skills \
//	    -index  /path/to/skills.bleve
//
// Each .md file becomes one Bleve document with the following fields:
//
//	id      — relative path from the skills root, e.g. "knowledge/SKILL.md"
//	title   — skill name extracted from the YAML frontmatter "name:" field,
//	           or the file stem if frontmatter is absent
//	content — full Markdown body (frontmatter stripped)
//	path    — absolute file path
//	skill   — parent directory name (e.g. "knowledge", "effect")
//
// If the index already exists at the target path it is deleted and rebuilt
// from scratch so the content is always fresh.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	bleve "github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
)

func main() {
	skillsDir := flag.String("skills", "", "root directory containing skill subdirectories (required)")
	indexPath := flag.String("index", "", "path for the Bleve index to create/overwrite (required)")
	flag.Parse()

	if *skillsDir == "" || *indexPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	abs, err := filepath.Abs(*skillsDir)
	if err != nil {
		log.Fatalf("resolve skills dir: %v", err)
	}

	docs, err := collectDocs(abs)
	if err != nil {
		log.Fatalf("collect docs: %v", err)
	}
	log.Printf("collected %d document(s) from %s", len(docs), abs)

	// Remove stale index.
	if err := os.RemoveAll(*indexPath); err != nil {
		log.Fatalf("remove old index: %v", err)
	}

	mapping := buildMapping()
	idx, err := bleve.New(*indexPath, mapping)
	if err != nil {
		log.Fatalf("create index: %v", err)
	}
	defer idx.Close()

	batch := idx.NewBatch()
	for _, d := range docs {
		if err := batch.Index(d.ID, d); err != nil {
			log.Fatalf("index %q: %v", d.ID, err)
		}
		log.Printf("  indexed: %s (%s)", d.ID, d.Title)
	}
	if err := idx.Batch(batch); err != nil {
		log.Fatalf("flush batch: %v", err)
	}

	count, _ := idx.DocCount()
	log.Printf("index built at %s  (%d documents)", *indexPath, count)
}

// doc is the schema for one indexed skill file.
type doc struct {
	ID      string `json:"id"`      // relative path  e.g. "knowledge/SKILL.md"
	Title   string `json:"title"`   // from frontmatter name: or file stem
	Content string `json:"content"` // full markdown body (frontmatter stripped)
	Path    string `json:"path"`    // absolute file path
	Skill   string `json:"skill"`   // parent directory name
}

// collectDocs walks skillsDir and returns one doc per .md file found.
func collectDocs(root string) ([]doc, error) {
	var docs []doc
	err := filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		if strings.ToLower(filepath.Ext(path)) != ".md" {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		skill := strings.SplitN(rel, string(filepath.Separator), 2)[0]

		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		title, body := parseFrontmatter(string(raw))
		if title == "" {
			// fall back to file stem
			title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}

		docs = append(docs, doc{
			ID:      rel,
			Title:   title,
			Content: body,
			Path:    path,
			Skill:   skill,
		})
		return nil
	})
	return docs, err
}

// parseFrontmatter extracts the "name:" value from YAML frontmatter (--- delimited)
// and returns (name, body).  If no frontmatter is present, name is empty and
// body is the entire input.
func parseFrontmatter(src string) (name, body string) {
	src = strings.TrimPrefix(src, "\xef\xbb\xbf") // strip BOM
	if !strings.HasPrefix(src, "---") {
		return "", src
	}
	// find closing ---
	rest := src[3:]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", src
	}
	fm := rest[:idx]
	body = strings.TrimPrefix(rest[idx+4:], "\n")

	scanner := bufio.NewScanner(strings.NewReader(fm))
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "name:"); ok {
			name = strings.TrimSpace(after)
			break
		}
	}
	return name, body
}

// buildMapping returns a Bleve index mapping with stored fields so that
// Document() can retrieve all values during Fetch.
func buildMapping() *mapping.IndexMappingImpl {
	m := bleve.NewIndexMapping()

	text := bleve.NewTextFieldMapping()
	text.Store = true
	text.Index = true

	keyword := bleve.NewKeywordFieldMapping()
	keyword.Store = true
	keyword.Index = true

	dm := bleve.NewDocumentMapping()
	dm.AddFieldMappingsAt("title", text)
	dm.AddFieldMappingsAt("content", text)
	dm.AddFieldMappingsAt("skill", keyword)
	dm.AddFieldMappingsAt("path", keyword)

	m.AddDocumentMapping("_default", dm)
	return m
}
