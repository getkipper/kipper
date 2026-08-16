package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
// It takes the whole entry because admissibility depends on more than the label:
// whether the name spells an address other than its own, and whether anything
// ever served under it, both decide what may be done with it. Startup is where a policy tightened
// after a snapshot was written gets applied, so a rule added to the registration
// guard alone would protect unused names while every name already taken under
// the old rule kept serving.
func (r *Registry) PruneEntries(drop func(*Entry) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	removed := 0
	for subdomain, entry := range r.entries {
		if drop(entry) {
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

	// Only set when something was actually migrated, so an idle gateway does not
	// rewrite its registry on every restart.
	migrated := false

	for i := range snap.Entries {
		entry := snap.Entries[i]
		// FirstProvenAt arrived with the tombstone, so a registration persisted
		// before it carries no value while its proof record shows the label
		// served. Reading that as never-proven would free the name the moment it
		// lapsed or was released, taking the tombstone away from exactly the
		// oldest clusters. A proof already recorded is the evidence the field
		// exists to hold, so adopt it.
		// The condition mirrors everProvenLocked: a lease naming no key
		// authorises nothing and must not buy a tombstone either, or a
		// half-written proof record becomes a 90-day hold.
		if entry.FirstProvenAt.IsZero() && !entry.ProvenAt.IsZero() && entry.ProofKeySPKI != "" {
			entry.FirstProvenAt = entry.ProvenAt
			// Mark it so the next flush records the migration. Left in memory it
			// is re-derived on every boot, which holds only while ProvenAt is
			// there to derive it from: a move clears ProvenAt, and an entry
			// migrated but never written comes back from a restart as never
			// served, losing the tombstone this exists to preserve.
			migrated = true
		}
		r.entries[entry.Subdomain] = &entry
		r.tokens[entry.Token] = entry.Subdomain
	}
	r.dirty = migrated

	return nil
}

// FlagEntries returns the subdomains matching report, sorted, without touching
// the registry. It exists for policy the gateway wants an operator to see but
// must not act on by itself, where acting on a false positive costs more than
// leaving it: the label-spells-another-address case cannot be told apart from a
// cluster that moved servers and kept its name.
func (r *Registry) FlagEntries(report func(*Entry) bool) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var flagged []string
	for subdomain, entry := range r.entries {
		if report(entry) {
			flagged = append(flagged, subdomain)
		}
	}
	sort.Strings(flagged)
	return flagged
}
