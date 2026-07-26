package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/chiragaggarwal/layby/internal/provider"
)

// Record is the locally cached view of a sandbox. It is a cache and never the
// source of truth: the provider's own tags are authoritative, so a lost laptop
// costs a reconciliation pass and not a leaked instance.
type Record struct {
	Handle    provider.Handle `json:"handle"`
	Blueprint string          `json:"blueprint"`
	ToolHash  string          `json:"tool_hash"`
}

// Store persists records to a single JSON file under the user's state
// directory.
type Store struct {
	path string
}

func OpenStore() (*Store, error) {
	base, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(base, ".layby")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}
	return &Store{path: filepath.Join(directory, "state.json")}, nil
}

func (s *Store) Load() ([]Record, error) {
	contents, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}

	records := []Record{}
	if err := json.Unmarshal(contents, &records); err != nil {
		return nil, fmt.Errorf("parsing state %s: %w", s.path, err)
	}
	return records, nil
}

func (s *Store) Save(records []Record) error {
	sort.Slice(records, func(first, second int) bool {
		return records[first].Handle.CreatedAt.Before(records[second].Handle.CreatedAt)
	})

	contents, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}

	// Write to a temporary file and rename so an interrupted write cannot
	// truncate the record of running sandboxes.
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, append(contents, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}
	return os.Rename(temporary, s.path)
}

func (s *Store) Add(record Record) error {
	records, err := s.Load()
	if err != nil {
		return err
	}
	filtered := make([]Record, 0, len(records)+1)
	for _, existing := range records {
		if existing.Handle.Identifier != record.Handle.Identifier {
			filtered = append(filtered, existing)
		}
	}
	return s.Save(append(filtered, record))
}

func (s *Store) Remove(identifier string) error {
	records, err := s.Load()
	if err != nil {
		return err
	}
	filtered := make([]Record, 0, len(records))
	for _, existing := range records {
		if existing.Handle.Identifier != identifier {
			filtered = append(filtered, existing)
		}
	}
	return s.Save(filtered)
}

func (s *Store) Find(identifier string) (Record, error) {
	records, err := s.Load()
	if err != nil {
		return Record{}, err
	}
	for _, record := range records {
		if record.Handle.Identifier == identifier {
			return record, nil
		}
	}
	return Record{}, fmt.Errorf("no sandbox %q in local state (try `layby doctor` to reconcile)", identifier)
}
