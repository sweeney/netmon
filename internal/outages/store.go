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
func (s *FileStore) Load() (*State, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%w: parsing %s: %v", ErrCorrupt, s.Path, err)
	}
	if err := st.valid(); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, s.Path, err)
	}
	return &st, nil
}

func (st State) valid() error {
	recs := st.Pending
	if st.Open != nil {
		recs = append(recs, *st.Open)
	}
	if len(recs) == 0 {
		return errors.New("no outages")
	}
	for i, r := range recs {
		if len(r.Transitions) == 0 || r.Transitions[0].At.IsZero() {
			return fmt.Errorf("outage %d has no start", i)
		}
	}
	for i, r := range st.Pending {
		if len(r.Transitions) < 2 {
			return fmt.Errorf("ended outage %d has no end", i)
		}
	}
	return nil
}

// Save implements Store. The file is written beside the target, synced and
// renamed into place, and the directory is synced too: this file exists for
// power loss, so it must survive one.
func (s *FileStore) Save(st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
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

func (NopStore) Load() (*State, error) { return nil, nil }
func (NopStore) Save(State) error      { return nil }
func (NopStore) Remove() error         { return nil }
