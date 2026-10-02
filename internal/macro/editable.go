// Editable-sites registry: which sites the operator has designated as
// agent-editable. This is an authority grant, so it lives entirely behind
// the admin band: the agent-facing API can read it (to refuse early) but has
// no endpoint that mutates it.
//
// Storage is one JSON file inside the macro store directory, written
// atomically with 0600 like everything else there.
package macro

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// registryFile lives inside the macro store directory.
const registryFile = "agent-editable-sites.json"

// EditableSite records one operator grant.
type EditableSite struct {
	Site      string    `json:"site"`
	GrantedBy string    `json:"granted_by"`
	GrantedAt time.Time `json:"granted_at"`
	// Note is an optional operator note ("practice site for macro work").
	Note string `json:"note,omitempty"`
}

// editableRegistry is the in-memory view of the on-disk registry.
type editableRegistry struct {
	mu     sync.Mutex
	dir    string // macro store dir; empty disables edits everywhere
	sites  map[string]EditableSite
	loaded bool
}

func openEditableRegistry(dir string) *editableRegistry {
	return &editableRegistry{dir: dir, sites: map[string]EditableSite{}}
}

func (r *editableRegistry) path() string {
	return filepath.Join(r.dir, registryFile)
}

// load reads the registry file once; a missing file is an empty registry.
// Callers hold r.mu.
func (r *editableRegistry) load() error {
	if r.loaded {
		return nil
	}
	r.loaded = true
	buf, err := os.ReadFile(r.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var sites []EditableSite
	if err := json.Unmarshal(buf, &sites); err != nil {
		return fmt.Errorf("editable-sites registry: %w", err)
	}
	for _, s := range sites {
		r.sites[normalizeSite(s.Site)] = s
	}
	return nil
}

// normalizeSite matches Site fields elsewhere: lower-case, no scheme, no
// trailing slash, no leading www. Recording already stores sites this way
// (see Recorder.AddEvent); the registry must agree with them.
func normalizeSite(site string) string {
	s := site
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(strings.ToLower(s), p) {
			s = s[len(p):]
			break
		}
	}
	s = strings.ToLower(s)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimPrefix(s, "www.")
	return s
}

// Enable grants agent-edit rights for a site. Admin band only.
func (r *editableRegistry) Enable(site, by, note string) error {
	if site == "" {
		return errors.New("site is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dir == "" {
		return errors.New("no macro store configured")
	}
	if err := r.load(); err != nil {
		return err
	}
	r.sites[normalizeSite(site)] = EditableSite{
		Site: normalizeSite(site), GrantedBy: by, GrantedAt: time.Now().UTC(), Note: note,
	}
	return r.saveLocked()
}

// Disable revokes the grant. Disabling never touches existing macros; it
// only closes the agent surface again.
func (r *editableRegistry) Disable(site string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dir == "" {
		return errors.New("no macro store configured")
	}
	if err := r.load(); err != nil {
		return err
	}
	delete(r.sites, normalizeSite(site))
	return r.saveLocked()
}

// Enabled reports whether a site is currently agent-editable. Read-only
// (the agent path calls this); missing/unreadable registry means false.
func (r *editableRegistry) Enabled(site string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.load() // unreadable registry must never read as "enabled"
	return r.sites[normalizeSite(site)].GrantedAt != (time.Time{})
}

// List returns all grants (admin dashboard).
func (r *editableRegistry) List() []EditableSite {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.load()
	out := make([]EditableSite, 0, len(r.sites))
	for _, s := range r.sites {
		out = append(out, s)
	}
	// deterministic order
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Site < out[j-1].Site; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// saveLocked persists atomically; caller holds r.mu.
func (r *editableRegistry) saveLocked() error {
	r.loaded = true
	sites := make([]EditableSite, 0, len(r.sites))
	for _, s := range r.sites {
		sites = append(sites, s)
	}
	// sort for stable files
	for i := 1; i < len(sites); i++ {
		for j := i; j > 0 && sites[j].Site < sites[j-1].Site; j-- {
			sites[j], sites[j-1] = sites[j-1], sites[j]
		}
	}
	buf, err := json.MarshalIndent(sites, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path() + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path())
}
