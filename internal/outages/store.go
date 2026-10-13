package outages

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrCorrupt marks a state file that was read but cannot be used. Unlike an
// I/O error, retrying will not help, so it is safe to discard.
var ErrCorrupt = errors.New("corrupt outage state")

// StateFileName is the file FileStore keeps in the state directory.
const StateFileName = "outage.json"

// FileStore keeps the record as JSON in a file. Adapted from ups-mqtt's.
type FileStore struct {
	Path string
}

// Load implements Store.
func (s *FileStore) Load() (*Record, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%w: parsing %s: %v", ErrCorrupt, s.Path, err)
	}
	if len(rec.Transitions) == 0 || rec.Transitions[0].At.IsZero() {
		return nil, fmt.Errorf("%w: parsing %s: no start", ErrCorrupt, s.Path)
	}
	return &rec, nil
}

// Save implements Store. The file is written beside the target, synced and
// renamed into place, and the directory is synced too: this file exists for
// power loss, so it must survive one.
func (s *FileStore) Save(rec Record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := writeSynced(tmp, append(data, '\n')); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	return syncDir(filepath.Dir(s.Path))
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close() //nolint:errcheck
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck
		return err
	}
	return f.Close()
}

// Remove implements Store.
func (s *FileStore) Remove() error {
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(s.Path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck
	return d.Sync()
}

// NopStore persists nothing; used when no state directory is configured.
type NopStore struct{}

func (NopStore) Load() (*Record, error) { return nil, nil }
func (NopStore) Save(Record) error      { return nil }
func (NopStore) Remove() error          { return nil }
