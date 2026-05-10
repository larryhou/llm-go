// Package config defines the configuration schema for llm-go.
// Config file location: ~/.config/llm/llm.json (or $XDG_CONFIG_HOME/llm/llm.json).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Info is the top-level config structure, matching opencode's Config.Info.
// Fields align with packages/opencode/src/config/config.ts.
type Info struct {
	Schema           string                    `json:"$schema,omitempty"`
	Shell            string                    `json:"shell,omitempty"`
	LogLevel         string                    `json:"logLevel,omitempty"` // DEBUG|INFO|WARN|ERROR
	Model            string                    `json:"model,omitempty"`    // "provider/model"
	SmallModel       string                    `json:"small_model,omitempty"`
	DefaultAgent     string                    `json:"default_agent,omitempty"`
	Username         string                    `json:"username,omitempty"`
	Snapshot         *bool                     `json:"snapshot,omitempty"`
	Share            string                    `json:"share,omitempty"` // manual|auto|disabled
	AutoUpdate       *BoolOrString             `json:"autoupdate,omitempty"`
	DisabledProviders []string                 `json:"disabled_providers,omitempty"`
	EnabledProviders  []string                 `json:"enabled_providers,omitempty"`
	Provider         map[string]*ProviderInfo  `json:"provider,omitempty"`
	Agent            map[string]*AgentInfo     `json:"agent,omitempty"`
	Instructions     []string                  `json:"instructions,omitempty"`
	Permission       *PermissionInfo           `json:"permission,omitempty"`
	Tools            map[string]bool           `json:"tools,omitempty"`
	ToolOutput       *ToolOutputConfig         `json:"tool_output,omitempty"`
	Compaction       *CompactionConfig         `json:"compaction,omitempty"`
	Experimental     *ExperimentalConfig       `json:"experimental,omitempty"`
	Enterprise       *EnterpriseConfig         `json:"enterprise,omitempty"`
	Watcher          *WatcherConfig            `json:"watcher,omitempty"`
}

// ProviderInfo matches opencode's ConfigProvider.Info.
type ProviderInfo struct {
	API       string                  `json:"api,omitempty"`
	Name      string                  `json:"name,omitempty"`
	Env       []string                `json:"env,omitempty"`
	ID        string                  `json:"id,omitempty"`
	NPM       string                  `json:"npm,omitempty"`
	Whitelist []string                `json:"whitelist,omitempty"`
	Blacklist []string                `json:"blacklist,omitempty"`
	Options   *ProviderOptions        `json:"options,omitempty"`
	Models    map[string]*ModelConfig `json:"models,omitempty"`
}

// ProviderOptions are SDK constructor options passed through to the provider.
type ProviderOptions struct {
	APIKey        string         `json:"apiKey,omitempty"`
	BaseURL       string         `json:"baseURL,omitempty"`
	EnterpriseURL string         `json:"enterpriseUrl,omitempty"`
	SetCacheKey   bool           `json:"setCacheKey,omitempty"`
	// Timeout in ms; nil = default 300000; pointer to -1 = disabled
	Timeout       *int           `json:"timeout,omitempty"`
	ChunkTimeout  int            `json:"chunkTimeout,omitempty"`
	Extra         map[string]any `json:"-"` // catch-all provider-specific options
}

func (p *ProviderOptions) UnmarshalJSON(data []byte) error {
	type plain ProviderOptions
	if err := json.Unmarshal(data, (*plain)(p)); err != nil {
		return err
	}
	// capture unknown fields into Extra
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	known := map[string]bool{
		"apiKey": true, "baseURL": true, "enterpriseUrl": true,
		"setCacheKey": true, "timeout": true, "chunkTimeout": true,
	}
	p.Extra = make(map[string]any)
	for k, v := range raw {
		if known[k] {
			continue
		}
		var val any
		if err := json.Unmarshal(v, &val); err == nil {
			p.Extra[k] = val
		}
	}
	return nil
}

// ModelConfig matches opencode's ConfigProvider.Model — per-model overrides.
type ModelConfig struct {
	ID          string          `json:"id,omitempty"`
	Name        string          `json:"name,omitempty"`
	Family      string          `json:"family,omitempty"`
	ReleaseDate string          `json:"release_date,omitempty"`
	Attachment  *bool           `json:"attachment,omitempty"`
	Reasoning   *bool           `json:"reasoning,omitempty"`
	Temperature *bool           `json:"temperature,omitempty"`
	ToolCall    *bool           `json:"tool_call,omitempty"`
	Interleaved *Interleaved    `json:"interleaved,omitempty"`
	Cost        *ModelCostConfig `json:"cost,omitempty"`
	Limit       *ModelLimitConfig `json:"limit,omitempty"`
	Modalities  *ModalityConfig `json:"modalities,omitempty"`
	Status      string          `json:"status,omitempty"`
	Options     map[string]any  `json:"options,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Variants    map[string]map[string]any `json:"variants,omitempty"`
}

// Interleaved can be true or {field: "reasoning_content"|"reasoning_details"}.
type Interleaved struct {
	Enabled bool
	Field   string // "reasoning_content" | "reasoning_details"
}

func (i *Interleaved) UnmarshalJSON(data []byte) error {
	// try bool first
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		i.Enabled = b
		return nil
	}
	var obj struct {
		Field string `json:"field"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	i.Enabled = true
	i.Field = obj.Field
	return nil
}

func (i Interleaved) MarshalJSON() ([]byte, error) {
	if i.Field == "" {
		return json.Marshal(i.Enabled)
	}
	return json.Marshal(map[string]string{"field": i.Field})
}

type ModelCostConfig struct {
	Input       float64              `json:"input"`
	Output      float64              `json:"output"`
	CacheRead   float64              `json:"cache_read,omitempty"`
	CacheWrite  float64              `json:"cache_write,omitempty"`
	Over200K    *ModelCostOver200K   `json:"context_over_200k,omitempty"`
}

type ModelCostOver200K struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

type ModelLimitConfig struct {
	Context int  `json:"context"`
	Input   *int `json:"input,omitempty"`
	Output  int  `json:"output"`
}

type ModalityConfig struct {
	Input  []string `json:"input,omitempty"`  // text|audio|image|video|pdf
	Output []string `json:"output,omitempty"`
}

// AgentInfo matches opencode's ConfigAgent.Info.
type AgentInfo struct {
	Model       string         `json:"model,omitempty"`
	MaxTokens   int            `json:"maxTokens,omitempty"`
	Prompt      string         `json:"prompt,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
	Tools       map[string]bool `json:"tools,omitempty"`
}

// PermissionInfo controls what tools/actions are allowed.
type PermissionInfo struct {
	// Rules maps tool names to permission rules
	Rules map[string]*PermissionRule `json:"rules,omitempty"`
}

type PermissionRule struct {
	Allowed *bool `json:"allowed,omitempty"`
}

// ToolOutputConfig matches opencode's tool_output config.
type ToolOutputConfig struct {
	MaxLines *int `json:"max_lines,omitempty"` // default 2000
	MaxBytes *int `json:"max_bytes,omitempty"` // default 51200
}

// CompactionConfig matches opencode's compaction config.
type CompactionConfig struct {
	Auto                 *bool `json:"auto,omitempty"`                    // default true
	Prune                *bool `json:"prune,omitempty"`                   // default false; set true to enable background tool-output pruning
	TailTurns            *int  `json:"tail_turns,omitempty"`              // default 2
	PreserveRecentTokens *int  `json:"preserve_recent_tokens,omitempty"`
	Reserved             *int  `json:"reserved,omitempty"`
}

// ExperimentalConfig holds experimental feature flags.
type ExperimentalConfig struct {
	DisablePasteSummary  bool     `json:"disable_paste_summary,omitempty"`
	BatchTool            bool     `json:"batch_tool,omitempty"`
	OpenTelemetry        bool     `json:"openTelemetry,omitempty"`
	PrimaryTools         []string `json:"primary_tools,omitempty"`
	ContinueLoopOnDeny   bool     `json:"continue_loop_on_deny,omitempty"`
	MCPTimeout           int      `json:"mcp_timeout,omitempty"` // ms
}

// EnterpriseConfig holds enterprise-specific settings.
type EnterpriseConfig struct {
	URL string `json:"url,omitempty"`
}

// WatcherConfig controls file watcher behaviour.
type WatcherConfig struct {
	Ignore []string `json:"ignore,omitempty"`
}

// BoolOrString handles autoupdate which can be bool or "notify".
type BoolOrString struct {
	Bool   *bool
	String string
}

func (b *BoolOrString) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		b.String = s
		return nil
	}
	var v bool
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	b.Bool = &v
	return nil
}

// Default constants matching opencode defaults.
const (
	DefaultToolOutputMaxLines = 2000
	DefaultToolOutputMaxBytes = 50 * 1024 // 51200

	DefaultCompactionTailTurns           = 2
	DefaultCompactionMinPreserveTokens   = 2_000
	DefaultCompactionMaxPreserveTokens   = 8_000
	DefaultCompactionBuffer              = 20_000
	DefaultProviderTimeout               = 300_000 // 5 minutes in ms
)

// Load reads the llm-go config from standard locations.
// Search order (later files merge/override earlier ones):
//  1. $XDG_CONFIG_HOME/llm/llm.json   (global user config)
//  2. ~/.config/llm/llm.json           (fallback when XDG_CONFIG_HOME is unset)
//  3. .llm/llm.json                    (project-local override)
func Load() (*Info, error) {
	paths := configPaths()
	cfg := &Info{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", p, err)
		}
	}
	return cfg, nil
}

// ConfigDir returns the global llm-go config directory.
// Value: $XDG_CONFIG_HOME/llm or ~/.config/llm
func ConfigDir() string {
	cfgHome := os.Getenv("XDG_CONFIG_HOME")
	if cfgHome == "" {
		home, _ := os.UserHomeDir()
		cfgHome = filepath.Join(home, ".config")
	}
	return filepath.Join(cfgHome, "llm")
}

func configPaths() []string {
	return []string{
		filepath.Join(ConfigDir(), "llm.json"),
		filepath.Join(".llm", "llm.json"),
	}
}

// GetCompactionAuto returns the effective auto-compaction setting (default true).
func (c *Info) GetCompactionAuto() bool {
	if c.Compaction == nil || c.Compaction.Auto == nil {
		return true
	}
	return *c.Compaction.Auto
}

// GetCompactionTailTurns returns the effective tail turns (default 2).
func (c *Info) GetCompactionTailTurns() int {
	if c.Compaction == nil || c.Compaction.TailTurns == nil {
		return DefaultCompactionTailTurns
	}
	return *c.Compaction.TailTurns
}

// GetToolOutputMaxLines returns the effective max lines for tool output (default 2000).
func (c *Info) GetToolOutputMaxLines() int {
	if c.ToolOutput == nil || c.ToolOutput.MaxLines == nil {
		return DefaultToolOutputMaxLines
	}
	return *c.ToolOutput.MaxLines
}

// GetToolOutputMaxBytes returns the effective max bytes for tool output (default 51200).
func (c *Info) GetToolOutputMaxBytes() int {
	if c.ToolOutput == nil || c.ToolOutput.MaxBytes == nil {
		return DefaultToolOutputMaxBytes
	}
	return *c.ToolOutput.MaxBytes
}
