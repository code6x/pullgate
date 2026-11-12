// Package state keeps root's state of one instance: the applied SHA, the
// questions, the marks of SHAs, an apply in progress (DESIGN.md sections 9
// and 10). It also holds the lock and the pause file.
package state

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/code6x/pullgate/internal/safedir"
)

// Marks of a SHA.
const (
	MarkAsked       = "asked"
	MarkExpired     = "expired"
	MarkSkipped     = "skipped"
	MarkCheckFailed = "check-failed"
	MarkRefused     = "refused"
	MarkFailed      = "failed"
)

// Question is one question that root asked.
type Question struct {
	ID      int64     `json:"id"`
	Code    string    `json:"code"`
	SHA     string    `json:"sha"`
	Base    string    `json:"base"`
	Asked   time.Time `json:"asked"`
	Expires time.Time `json:"expires"`
	Open    bool      `json:"open"`
	Auto    bool      `json:"auto,omitempty"`
}

// Mark is what pullgate knows about one SHA.
type Mark struct {
	Mark  string    `json:"mark"`
	Tries int       `json:"tries,omitempty"`
	At    time.Time `json:"at"`
}

// InProgress is an apply or a rollback that started and has not ended.
type InProgress struct {
	Phase   string    `json:"phase"` // "apply" or "rollback"
	SHA     string    `json:"sha"`
	OldSHA  string    `json:"old_sha"`
	Started time.Time `json:"started"`
}

// State is the content of state.json.
type State struct {
	Version    int              `json:"version"`
	Applied    string           `json:"applied"`
	AppliedAt  time.Time        `json:"applied_at"`
	Held       string           `json:"held,omitempty"`
	LastTip    string           `json:"last_tip,omitempty"`
	Questions  []Question       `json:"questions"`
	Marks      map[string]*Mark `json:"marks"`
	InProgress *InProgress      `json:"in_progress,omitempty"`
	AskTimes   []time.Time      `json:"ask_times,omitempty"`
	LimitNote  time.Time        `json:"limit_note,omitempty"`
	CheckAt    time.Time        `json:"check_at,omitzero"`   // the last check request that was taken (DESIGN.md section 8)
	CheckNote  time.Time        `json:"check_note,omitzero"` // the last "too many check requests" reply
	NextID     int64            `json:"next_id"`
}

// MaxMarks is the most SHA marks kept; the oldest go first.
const MaxMarks = 500

// Open returns the open question, or nil.
func (s *State) Open() *Question {
	for i := range s.Questions {
		if s.Questions[i].Open {
			return &s.Questions[i]
		}
	}
	return nil
}

// FindCode returns the index of the question with code, or -1. The compare
// takes the same time for every question.
func (s *State) FindCode(code string) int {
	found := -1
	for i := range s.Questions {
		if subtle.ConstantTimeCompare([]byte(s.Questions[i].Code), []byte(code)) == 1 {
			found = i
		}
	}
	return found
}

// Remove removes the question at index i.
func (s *State) Remove(i int) Question {
	q := s.Questions[i]
	s.Questions = append(s.Questions[:i], s.Questions[i+1:]...)
	return q
}

// SetMark sets the mark of sha, and keeps the try count.
func (s *State) SetMark(sha, mark string, now time.Time) *Mark {
	if s.Marks == nil {
		s.Marks = map[string]*Mark{}
	}
	m := s.Marks[sha]
	if m == nil {
		m = &Mark{}
		s.Marks[sha] = m
	}
	m.Mark = mark
	m.At = now
	s.prune()
	return m
}

func (s *State) prune() {
	if len(s.Marks) <= MaxMarks {
		return
	}
	type kv struct {
		k string
		t time.Time
	}
	var all []kv
	for k, m := range s.Marks {
		all = append(all, kv{k, m.At})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for _, e := range all[:len(all)-MaxMarks] {
		delete(s.Marks, e.k)
	}
}

// NewCode returns a random one-time code: 128 bits, 22 characters.
func NewCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Dir is the state directory of one instance.
type Dir struct {
	Path string
}

func (d Dir) file(n string) string { return filepath.Join(d.Path, n) }

// StateFile is the path of state.json.
func (d Dir) StateFile() string { return d.file("state.json") }

// PauseFile is the path of the pause file.
func (d Dir) PauseFile() string { return d.file("paused") }

// AutoFile is the path of the auto approval.
func (d Dir) AutoFile() string { return d.file("auto-approval") }

// Repo is the path of the bare clone.
func (d Dir) Repo() string { return d.file("repo.git") }

// Trees is the directory of the trees.
func (d Dir) Trees() string { return d.file("trees") }

// Ensure makes the state directory (0711) and the trees directory. The
// directories above must pass the check of rule 6.
func (d Dir) Ensure() error {
	if err := safedir.CheckParents(filepath.Dir(d.Path), uint32(os.Geteuid())); err != nil { //nolint:gosec // a uid fits in uint32
		return fmt.Errorf("the state directory is not safe: %w", err)
	}
	for _, p := range []string{d.Path, d.Trees()} {
		if err := os.Mkdir(p, 0o711); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("make %s: %w", p, err)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory. Fix: remove it", p)
		}
		if err := os.Chmod(p, 0o711); err != nil {
			return err
		}
	}
	return nil
}

// Paused reports whether the pause file exists.
func (d Dir) Paused() bool {
	_, err := os.Lstat(d.PauseFile())
	return err == nil
}

// Pause writes the pause file with a reason.
func (d Dir) Pause(reason string) error {
	return WriteFileAtomic(d.PauseFile(), []byte(reason+"\n"), 0o644)
}

// Resume removes the pause file.
func (d Dir) Resume() error {
	err := os.Remove(d.PauseFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Lock takes the lock of the instance. With wait false it fails at once
// when another process holds it. Close the file to free it.
func (d Dir) Lock(wait bool) (*os.File, error) {
	f, err := os.OpenFile(d.file("lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the lock: %w", err)
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("take the lock: %w", err)
	}
	return f, nil
}

// ErrBusy means another process holds the lock.
var ErrBusy = errors.New("another pullgate process holds the lock of this instance")

// Load reads the state. A missing file is an empty state.
func (d Dir) Load() (*State, error) {
	b, err := os.ReadFile(d.StateFile())
	if errors.Is(err, fs.ErrNotExist) {
		return &State{Version: 1, Marks: map[string]*Mark{}, NextID: 1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the state: %w", err)
	}
	s := &State{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(s); err != nil {
		return nil, fmt.Errorf("the state %s is broken: %w. Fix: check the host, then remove the file (pullgate starts again as on the first run)", d.StateFile(), err)
	}
	if s.Version != 1 {
		return nil, fmt.Errorf("the state %s has version %d, not 1", d.StateFile(), s.Version)
	}
	if s.Marks == nil {
		s.Marks = map[string]*Mark{}
	}
	return s, nil
}

// Save writes the state atomically and syncs it.
func (d Dir) Save(s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(d.StateFile(), append(b, '\n'), 0o600)
}

// WriteFileAtomic writes a file in a directory that only root can write:
// a temporary file, sync, rename, sync of the directory.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if df, err := os.Open(dir); err == nil { //nolint:gosec // our own directory
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
}
