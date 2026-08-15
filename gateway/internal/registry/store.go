package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// valueSnapshot is the on-disk format for the registry state. Entries are held
// by value so a save marshals an independent copy, never a live pointer.
type valueSnapshot struct {
	Entries []Entry `json:"entries"`
}

// SaveTo persists the current registry state to a JSON file.
// The write is atomic — it writes to a temp file then renames.
func (r *Registry) SaveTo(path string) error {
	// Hold saveMu across the whole snapshot-to-rename so concurrent saves commit
	// in the order they run: the save that renames last also snapshotted last, so
	// an older snapshot can never overwrite newer state on disk. It also gives
	// each save a unique temp file, so two writers can't interleave into one
	// fixed temp path and rename a half-written file over the registry (which the
	// next start would refuse to parse).
	r.saveMu.Lock()
	defer r.saveMu.Unlock()

	// Snapshot entries by value under the read lock. Marshalling shared pointers
	// after releasing it would race Register/Ping mutating LastSeen.
	r.mu.RLock()
	entries := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, *e)
	}
	r.mu.RUnlock()

	data, err := json.MarshalIndent(valueSnapshot{Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling registry: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "registry-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}

	return nil
}

// FlushIfDirty persists the registry only if something set the dirty flag — a
// Ping-advanced LastSeen or a pin-state transition — and reports whether a
// write happened. Frequent updates set the flag rather than rewriting the
// whole snapshot on every call; a periodic flush coalesces them into at most
// one write per interval. On a write failure the state stays marked dirty so
// the next flush retries.
func (r *Registry) FlushIfDirty(path string) (bool, error) {
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return false, nil
	}
	// Clear before the write. A Ping arriving during SaveTo either lands in this
	// snapshot or re-sets dirty for the next flush, so no update is lost.
	r.dirty = false
	r.mu.Unlock()

	if err := r.SaveTo(path); err != nil {
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()
		return false, err
	}
	return true, nil
}

// Prune drops every entry whose IP fails keep and returns how many were
// removed. It lets the caller re-apply the current public-IP policy to state
// that was persisted under an older, looser policy, so a stale private target
// can't survive a restart and become an SSRF pivot.
func (r *Registry) Prune(keep func(ip string) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	removed := 0
	for subdomain, entry := range r.entries {
		if !keep(entry.IP) {
			delete(r.entries, subdomain)
			delete(r.tokens, entry.Token)
			removed++
		}
	}
	return removed
}

// PruneEntries drops every entry whose subdomain and address fail keep, and
// returns how many were removed.
//
// It takes both because a label's admissibility depends on both: a name spelling
// an IP address is registrable by that address and no other, which Prune (which
// sees only the address) cannot express. Startup is where a policy tightened
// after a snapshot was written gets applied, so a rule added to the registration
// guard alone would protect unused names while every name already taken under
// the old rule kept serving.
func (r *Registry) PruneEntries(keep func(subdomain, ip string) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	removed := 0
	for subdomain, entry := range r.entries {
		if !keep(subdomain, entry.IP) {
			delete(r.entries, subdomain)
			delete(r.tokens, entry.Token)
			removed++
		}
	}
	return removed
}

// LoadFrom restores registry state from a JSON file.
// If the file does not exist, the registry starts empty (not an error).
func (r *Registry) LoadFrom(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // path from config, not user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading registry file: %w", err)
	}

	var snap valueSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("parsing registry file: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.entries = make(map[string]*Entry, len(snap.Entries))
	r.tokens = make(map[string]string, len(snap.Entries))

	for i := range snap.Entries {
		entry := snap.Entries[i]
		r.entries[entry.Subdomain] = &entry
		r.tokens[entry.Token] = entry.Subdomain
	}

	return nil
}
