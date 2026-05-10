package tool

import (
	"fmt"
	"sync"
)

// Registry holds all registered tools.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool to the registry.
// Panics if a tool with the same name already exists.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name()]; exists {
		panic(fmt.Sprintf("tool registry: duplicate tool name %q", t.Name()))
	}
	r.tools[t.Name()] = t
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// All returns all registered tools.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}

// Filter returns tools whose names are in the allowed set.
// If allowed is nil, all tools are returned.
func (r *Registry) Filter(allowed map[string]bool) []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if allowed == nil {
		out := make([]Tool, 0, len(r.tools))
		for _, t := range r.tools {
			out = append(out, t)
		}
		return out
	}
	var out []Tool
	for name, enabled := range allowed {
		if enabled {
			if t, ok := r.tools[name]; ok {
				out = append(out, t)
			}
		}
	}
	return out
}
