package selfupdate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/code6x/pullgate/internal/state"
)

// Question is the open update question. Root takes the version and the
// SHA-256 from here on Apply, never from the approval.
type Question struct {
	ID      int64     `json:"id"`
	Code    string    `json:"code"`
	Version string    `json:"version"`
	SHA256  string    `json:"sha256"`
	Asked   time.Time `json:"asked"`
	Expires time.Time `json:"expires"`
}

// InProgress is an update that has started and not ended. A crash leaves
// it in the state, and the next start reports it.
type InProgress struct {
	Version string    `json:"version"`
	From    string    `json:"from"`
	Started time.Time `json:"started"`
}

// State is /var/lib/pullgate/_update/state.json.
type State struct {
	Version    int                  `json:"version"`
	NextID     int64                `json:"next_id"`
	Question   *Question            `json:"question,omitempty"`
	Notified   map[string]time.Time `json:"notified,omitempty"` // release → its last notice
	Warned     map[string]time.Time `json:"warned,omitempty"`   // release → its "failed a check" notice
	CheckAt    time.Time            `json:"check_at,omitzero"`  // the last /update that was taken
	CheckNote  time.Time            `json:"check_note,omitzero"`
	InProgress *InProgress          `json:"in_progress,omitempty"`
}

func (u *Updater) stateFile() string { return filepath.Join(u.StateDir, "state.json") }

func (u *Updater) load() (*State, error) {
	b, err := os.ReadFile(u.stateFile())
	if errors.Is(err, fs.ErrNotExist) {
		// IDs from the clock: after the file was removed, a new question
		// never has the ID of one that the listener still knows.
		return &State{Version: 1, NextID: u.now().Unix()}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the update state: %w", err)
	}
	s := &State{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	err = dec.Decode(s)
	if err == nil && s.Version != 1 {
		err = fmt.Errorf("version %d, not 1", s.Version)
	}
	if err != nil {
		return nil, fmt.Errorf("the update state %s is broken: %w. Fix: remove it; an open update question then stops working, and /update asks again", u.stateFile(), err)
	}
	return s, nil
}

// save writes the state, synced to the disk.
func (u *Updater) save(s *State) error {
	// Keep only the releases newer than this version: older ones are done.
	for _, m := range []map[string]time.Time{s.Notified, s.Warned} {
		for v := range m {
			if !newer(v, u.Version) {
				delete(m, v)
			}
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(u.stateFile(), append(b, '\n'), 0o600)
}
