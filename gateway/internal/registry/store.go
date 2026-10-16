package registry

import (
	"encoding/json"
	"errors"
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
	// Serialize snapshots and writes so an older snapshot cannot overwrite
	// a newer one on disk.
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	return r.saveLocked(path)
}

// ErrNotRecorded reports a registration that could not be saved.
var ErrNotRecorded = errors.New("the registration could not be recorded")

// RegisterDurably calls Register and saves newly created registrations. It holds
// saveMu through registration, save, and rollback to exclude concurrent saves.
// A save failure triggers deregistration and returns ErrNotRecorded, including
// any rollback error. Callers must save changes to existing registrations.
func (r *Registry) RegisterDurably(subdomain, ip, token, path string) (*Entry, Outcome, error) {
	r.saveMu.Lock()
	defer r.saveMu.Unlock()

	entry, outcome, err := r.Register(subdomain, ip, token)
	if err != nil || outcome != Created {
		return entry, outcome, err
	}
	if serr := r.saveLocked(path); serr != nil {
		if _, derr := r.Deregister(entry.Token); derr != nil {
			return nil, outcome, fmt.Errorf("%w: %v; withdrawing it also failed: %v", ErrNotRecorded, serr, derr)
		}
		return nil, outcome, fmt.Errorf("%w: %v", ErrNotRecorded, serr)
	}
	return entry, outcome, nil
}

// saveLocked writes a snapshot to disk. The caller must hold saveMu.
func (r *Registry) saveLocked(path string) error {
	// Snapshot entries by value under the read lock. Marshaling shared pointers
	// after releasing it would race Register/Ping mutating LastSeen.
	r.mu.RLock()
	entries := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, *e)
	}
	r.mu.RUnlock()

	data, err := json.MarshalIndent(valueSnapshot{Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling registry: %w", err)
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

// PruneEntries removes entries for which drop returns true and returns the count.
// The predicate receives the whole entry so startup policy can consider serving
// history as well as the label and address.
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
		// Backfill serving history in older snapshots from a proof with a key,
		// preserving tombstone eligibility for existing clusters.
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
