package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/larryhou/llm-go/config"
)

func TestDefaultValues(t *testing.T) {
	cfg := &config.Info{}
	if cfg.GetCompactionAuto() != true {
		t.Error("GetCompactionAuto default should be true")
	}
	if cfg.GetCompactionTailTurns() != config.DefaultCompactionTailTurns {
		t.Errorf("GetCompactionTailTurns default = %d, want %d",
			cfg.GetCompactionTailTurns(), config.DefaultCompactionTailTurns)
	}
	if cfg.GetToolOutputMaxLines() != config.DefaultToolOutputMaxLines {
		t.Errorf("GetToolOutputMaxLines default = %d, want %d",
			cfg.GetToolOutputMaxLines(), config.DefaultToolOutputMaxLines)
	}
	if cfg.GetToolOutputMaxBytes() != config.DefaultToolOutputMaxBytes {
		t.Errorf("GetToolOutputMaxBytes default = %d, want %d",
			cfg.GetToolOutputMaxBytes(), config.DefaultToolOutputMaxBytes)
	}
}

func TestCompactionOverrides(t *testing.T) {
	autoFalse := false
	tailTurns := 5
	preserveTokens := 6000
	reserved := 15000
	cfg := &config.Info{
		Compaction: &config.CompactionConfig{
			Auto:                 &autoFalse,
			TailTurns:            &tailTurns,
			PreserveRecentTokens: &preserveTokens,
			Reserved:             &reserved,
		},
	}
	if cfg.GetCompactionAuto() != false {
		t.Error("GetCompactionAuto should respect override false")
	}
	if cfg.GetCompactionTailTurns() != 5 {
		t.Errorf("GetCompactionTailTurns = %d, want 5", cfg.GetCompactionTailTurns())
	}
}

func TestToolOutputOverrides(t *testing.T) {
	maxLines := 500
	maxBytes := 10240
	cfg := &config.Info{
		ToolOutput: &config.ToolOutputConfig{
			MaxLines: &maxLines,
			MaxBytes: &maxBytes,
		},
	}
	if cfg.GetToolOutputMaxLines() != 500 {
		t.Errorf("GetToolOutputMaxLines = %d, want 500", cfg.GetToolOutputMaxLines())
	}
	if cfg.GetToolOutputMaxBytes() != 10240 {
		t.Errorf("GetToolOutputMaxBytes = %d, want 10240", cfg.GetToolOutputMaxBytes())
	}
}

func TestBoolOrString_bool(t *testing.T) {
	var b config.BoolOrString
	if err := json.Unmarshal([]byte(`true`), &b); err != nil {
		t.Fatal(err)
	}
	if b.Bool == nil || *b.Bool != true {
		t.Error("expected Bool=true")
	}
	if b.String != "" {
		t.Error("expected empty String")
	}
}

func TestBoolOrString_string(t *testing.T) {
	var b config.BoolOrString
	if err := json.Unmarshal([]byte(`"notify"`), &b); err != nil {
		t.Fatal(err)
	}
	if b.String != "notify" {
		t.Errorf("expected String=notify, got %q", b.String)
	}
}

func TestProviderOptions_extraFields(t *testing.T) {
	raw := `{
		"apiKey": "sk-test",
		"baseURL": "http://localhost:11434/v1",
		"timeout": 60000,
		"customHeader": "value1",
		"anotherKey": 42
	}`
	var opts config.ProviderOptions
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		t.Fatal(err)
	}
	if opts.APIKey != "sk-test" {
		t.Errorf("APIKey = %q, want sk-test", opts.APIKey)
	}
	if opts.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("BaseURL = %q", opts.BaseURL)
	}
	if opts.Extra["customHeader"] != "value1" {
		t.Errorf("Extra[customHeader] = %v", opts.Extra["customHeader"])
	}
	if opts.Extra["apiKey"] != nil {
		t.Error("known fields should not appear in Extra")
	}
}

func TestInterleaved_bool(t *testing.T) {
	var i config.Interleaved
	if err := json.Unmarshal([]byte(`true`), &i); err != nil {
		t.Fatal(err)
	}
	if !i.Enabled || i.Field != "" {
		t.Errorf("expected Enabled=true Field='', got %+v", i)
	}
}

func TestInterleaved_object(t *testing.T) {
	var i config.Interleaved
	if err := json.Unmarshal([]byte(`{"field":"reasoning_content"}`), &i); err != nil {
		t.Fatal(err)
	}
	if !i.Enabled || i.Field != "reasoning_content" {
		t.Errorf("expected Enabled=true Field=reasoning_content, got %+v", i)
	}
}

func TestInterleaved_roundtrip(t *testing.T) {
	i := config.Interleaved{Enabled: true, Field: "reasoning_details"}
	b, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	var i2 config.Interleaved
	if err := json.Unmarshal(b, &i2); err != nil {
		t.Fatal(err)
	}
	if i2.Field != "reasoning_details" {
		t.Errorf("roundtrip field = %q, want reasoning_details", i2.Field)
	}
}

func TestLoad_missingFile(t *testing.T) {
	// Point config search at a temp dir with no config files
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load with no config file should not error, got: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load should return non-nil cfg even with no files")
	}
}

func TestLoad_parsesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	opencodeDir := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(opencodeDir, 0o750); err != nil {
		t.Fatal(err)
	}
	content := `{
		"model": "anthropic/claude-sonnet-4-5",
		"compaction": {"auto": false, "tail_turns": 3}
	}`
	if err := os.WriteFile(filepath.Join(opencodeDir, "opencode.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Model != "anthropic/claude-sonnet-4-5" {
		t.Errorf("Model = %q, want anthropic/claude-sonnet-4-5", cfg.Model)
	}
	if cfg.GetCompactionAuto() != false {
		t.Error("compaction.auto should be false")
	}
	if cfg.GetCompactionTailTurns() != 3 {
		t.Errorf("tail_turns = %d, want 3", cfg.GetCompactionTailTurns())
	}
}

func TestDefaultConstants(t *testing.T) {
	if config.DefaultCompactionBuffer != 20_000 {
		t.Errorf("DefaultCompactionBuffer = %d, want 20000", config.DefaultCompactionBuffer)
	}
	if config.DefaultCompactionTailTurns != 2 {
		t.Errorf("DefaultCompactionTailTurns = %d, want 2", config.DefaultCompactionTailTurns)
	}
	if config.DefaultToolOutputMaxLines != 2000 {
		t.Errorf("DefaultToolOutputMaxLines = %d, want 2000", config.DefaultToolOutputMaxLines)
	}
	if config.DefaultToolOutputMaxBytes != 50*1024 {
		t.Errorf("DefaultToolOutputMaxBytes = %d, want 51200", config.DefaultToolOutputMaxBytes)
	}
}
