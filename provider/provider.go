// Package provider defines the runtime provider/model registry.
// Aligned with packages/opencode/src/provider/provider.ts.
package provider

import (
	"fmt"
	"strings"

	"github.com/larryhou/llm-go/llm"
)

// Capabilities describes what a model supports.
type Capabilities struct {
	Temperature bool
	Reasoning   bool
	Attachment  bool
	ToolCall    bool
	Interleaved *Interleaved
	Input       Modalities
	Output      Modalities
}

// Interleaved describes interleaved reasoning output mode.
type Interleaved struct {
	// Field is "reasoning_content" or "reasoning_details"; empty means plain boolean true.
	Field string
}

// Modalities describes supported input/output modalities.
type Modalities struct {
	Text  bool
	Audio bool
	Image bool
	Video bool
	PDF   bool
}

// APIInfo holds the provider SDK endpoint info for a model.
type APIInfo struct {
	ID  string // model ID sent to the SDK (may differ from logical ID)
	URL string // base API URL
	NPM string // npm package (e.g. "@ai-sdk/anthropic") — informational only
}

// Model is the runtime representation of an LLM model.
// Aligned with Provider.Model in provider.ts.
type Model struct {
	llm.Model                  // embeds ID, ProviderID, APIID, Limit, Cost
	API          APIInfo
	Name         string
	Family       string
	ReleaseDate  string
	Status       string // active | deprecated | alpha
	Capabilities Capabilities
	Headers      map[string]string
	Variants     map[string]map[string]any // reasoning effort presets
}

// Info is the runtime provider descriptor.
// Aligned with Provider.Info in provider.ts.
type Info struct {
	ID     string
	Name   string
	Source string // env | config | custom | api
	Env    []string
	Key    string
	Options map[string]any
	Models  map[string]*Model
}

// Registry holds all registered providers.
type Registry struct {
	providers map[string]*Info
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]*Info)}
}

// Register adds or replaces a provider.
func (r *Registry) Register(p *Info) {
	r.providers[p.ID] = p
}

// Get returns the provider info for a given ID.
func (r *Registry) Get(id string) (*Info, bool) {
	p, ok := r.providers[id]
	return p, ok
}

// GetModel looks up a model by "providerID/modelID".
func (r *Registry) GetModel(providerID, modelID string) (*Model, error) {
	p, ok := r.providers[providerID]
	if !ok {
		return nil, fmt.Errorf("provider %q not found", providerID)
	}
	m, ok := p.Models[modelID]
	if !ok {
		return nil, fmt.Errorf("model %q not found in provider %q", modelID, providerID)
	}
	return m, nil
}

// ParseModel splits "providerID/modelID" into its parts.
// Aligned with provider.ts parseModel().
func ParseModel(s string) (providerID, modelID string, err error) {
	idx := strings.Index(s, "/")
	if idx < 0 {
		return "", "", fmt.Errorf("invalid model string %q: expected provider/model", s)
	}
	return s[:idx], s[idx+1:], nil
}

// List returns all registered providers.
func (r *Registry) List() map[string]*Info {
	out := make(map[string]*Info, len(r.providers))
	for k, v := range r.providers {
		out[k] = v
	}
	return out
}
