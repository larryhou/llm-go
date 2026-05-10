// Package auth handles reading and writing opencode's auth.json.
// Aligned with packages/opencode/src/auth/index.ts.
//
// Storage location: $XDG_DATA_HOME/opencode/auth.json (chmod 0600)
package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const fileMode = 0o600

// Type discriminator values.
const (
	TypeOauth    = "oauth"
	TypeAPI      = "api"
	TypeWellKnown = "wellknown"
)

// Info is the union of all auth types, discriminated by Type field.
// Matches opencode's Auth.Info = Oauth | Api | WellKnown.
type Info struct {
	Type string `json:"type"`

	// Oauth fields
	Refresh       string `json:"refresh,omitempty"`
	Access        string `json:"access,omitempty"`
	Expires       int64  `json:"expires,omitempty"` // unix ms, NonNegativeInt
	AccountID     string `json:"accountId,omitempty"`
	EnterpriseURL string `json:"enterpriseUrl,omitempty"`

	// Api fields
	Key      string            `json:"key,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`

	// WellKnown fields (Key reused above)
	Token string `json:"token,omitempty"`
}

// IsOauth returns true if this is an OAuth token.
func (i *Info) IsOauth() bool { return i.Type == TypeOauth }

// IsAPI returns true if this is an API key.
func (i *Info) IsAPI() bool { return i.Type == TypeAPI }

// IsWellKnown returns true if this is a well-known token.
func (i *Info) IsWellKnown() bool { return i.Type == TypeWellKnown }

// GetKey returns the effective API key regardless of auth type.
func (i *Info) GetKey() string {
	switch i.Type {
	case TypeAPI:
		return i.Key
	case TypeWellKnown:
		return i.Token
	case TypeOauth:
		return i.Access
	}
	return ""
}

// Store holds all provider auth entries.
// auth.json is Record<providerID, Info>.
type Store struct {
	path    string
	entries map[string]*Info
}

// Load reads auth.json from the standard opencode data directory.
// Returns an empty store if the file doesn't exist.
func Load() (*Store, error) {
	p := storePath()
	s := &Store{path: p, entries: make(map[string]*Info)}

	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}

	if err := json.Unmarshal(data, &s.entries); err != nil {
		return nil, err
	}
	return s, nil
}

// Get returns the auth info for a provider, or nil if not found.
func (s *Store) Get(providerID string) *Info {
	return s.entries[providerID]
}

// Set stores auth info for a provider and persists to disk.
func (s *Store) Set(providerID string, info *Info) error {
	s.entries[providerID] = info
	return s.save()
}

// Delete removes auth info for a provider and persists to disk.
func (s *Store) Delete(providerID string) error {
	delete(s.entries, providerID)
	return s.save()
}

// All returns all stored entries.
func (s *Store) All() map[string]*Info {
	out := make(map[string]*Info, len(s.entries))
	for k, v := range s.entries {
		out[k] = v
	}
	return out
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, fileMode)
}

// storePath returns the auth.json path.
// $XDG_DATA_HOME/opencode/auth.json or ~/.local/share/opencode/auth.json
func storePath() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, _ := os.UserHomeDir()
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "opencode", "auth.json")
}

// ResolveKey looks up the API key for a provider using the standard priority order:
//  1. Environment variables from envVars list
//  2. auth.json entry
func ResolveKey(providerID string, envVars []string, store *Store) (key, source string) {
	// 1. Environment variables
	for _, env := range envVars {
		if v := os.Getenv(env); v != "" {
			return v, "env"
		}
	}

	// 2. auth.json
	if store != nil {
		if info := store.Get(providerID); info != nil {
			if k := info.GetKey(); k != "" {
				return k, "api"
			}
		}
	}

	return "", ""
}
