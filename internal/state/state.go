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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
	// MarkNothing: the check exited 3, "nothing to apply on this host"
	// (DESIGN.md section 7). The applied SHA stays as it is.
	MarkNothing = "nothing"
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

// Broken is the state after an apply failed and its rollback failed too
// (DESIGN.md section 10). Only a successful fix forward, or root, ends it.
type Broken struct {
	Since  time.Time `json:"since"`
	SHA    string    `json:"sha"`     // the last commit that failed; a fix forward descends from it
	OldSHA string    `json:"old_sha"` // the applied commit that the failed rollback tried to restore
	Reason string    `json:"reason"`  // the last error, as plain text
}

// State is the content of state.json.
type State struct {
	Version    int              `json:"version"`
	Applied    string           `json:"applied"`
	AppliedAt  time.Time        `json:"applied_at"`
	Held       string           `json:"held,omitempty"`
	LastTip    string           `json:"last_tip,omitempty"`
	TipSince   time.Time        `json:"tip_since,omitzero"` // when root first saw LastTip as the tip ([limits] settle)
	Questions  []Question       `json:"questions"`
	Marks      map[string]*Mark `json:"marks"`
	InProgress *InProgress      `json:"in_progress,omitempty"`
	Broken     *Broken          `json:"broken,omitempty"`
	AskTimes   []time.Time      `json:"ask_times,omitempty"`
	LimitNote  time.Time        `json:"limit_note,omitempty"`
	CheckAt    time.Time        `json:"check_at,omitzero"`   // the last check request that was taken (DESIGN.md section 8)
	CheckNote  time.Time        `json:"check_note,omitzero"` // the last "too many check requests" reply
	StatusAt   time.Time        `json:"status_at,omitzero"`  // the last /status that was answered
	PauseNote  time.Time        `json:"pause_note,omitzero"` // the last reply to /pause
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

// PauseIfNone writes the pause file only if there is none. It never
// replaces one, even one that appears at the same time (a hard link
// fails when the name exists). It reports whether it wrote it.
func (d Dir) PauseIfNone(reason string) (bool, error) {
	f, err := os.CreateTemp(filepath.Dir(d.PauseFile()), ".tmp-")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, werr := f.WriteString(reason + "\n")
	if werr == nil {
		werr = f.Chmod(0o644)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return false, werr
	}
	if err := os.Link(tmp, d.PauseFile()); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// TelegramPause starts the reason of a pause that came from /pause
// (code6x/pullgate#32). No other reason starts with it.
const TelegramPause = "paused from Telegram at "

// PauseReason is the first line of the pause file, at most 200 bytes, or
// "" when there is none. The file is root's; it is never followed.
func (d Dir) PauseReason() string {
	f, err := os.OpenFile(d.PauseFile(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 200)
	n, _ := io.ReadFull(f, b)
	line, _, _ := strings.Cut(string(b[:n]), "\n")
	return line
}

// telegramPauseRE is the whole first line of a pause from /pause.
var telegramPauseRE = regexp.MustCompile(`^` + regexp.QuoteMeta(TelegramPause) + `[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2} UTC$`)

// ResumeTelegram removes the pause file only if it is a /pause one
// (TelegramPaused). It moves the file aside first and checks that the file
// it moved is the one it checked; if not (root wrote a new pause in
// between), it puts that one back without replacing anything, and keeps
// the pause. It reports whether the pause was lifted.
func (d Dir) ResumeTelegram(uid uint32) (bool, error) {
	f, err := os.OpenFile(d.PauseFile(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, nil
	}
	defer func() { _ = f.Close() }()
	checked, err := f.Stat()
	if err != nil || !telegramPauseFile(f, checked, uid) {
		return false, nil
	}
	tmp := d.file(".resume-tmp")
	_ = os.Remove(tmp)
	if err := os.Rename(d.PauseFile(), tmp); err != nil {
		return false, err
	}
	moved, err := os.Lstat(tmp)
	if err != nil || !os.SameFile(checked, moved) {
		// Not the file that was checked: put it back, never over a new one.
		if lerr := os.Link(tmp, d.PauseFile()); lerr != nil && !errors.Is(lerr, fs.ErrExist) {
			return false, lerr
		}
		_ = os.Remove(tmp)
		return false, nil
	}
	return true, os.Remove(tmp)
}

// TelegramPaused reports whether the pause file is one that /pause wrote:
// a regular file of uid, not linked twice, of one line that is exactly the
// reason of /pause. Any other pause (by pullgate itself or by root) is not.
func (d Dir) TelegramPaused(uid uint32) bool {
	f, err := os.OpenFile(d.PauseFile(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return telegramPauseFile(f, fi, uid)
}

func telegramPauseFile(f *os.File, fi os.FileInfo, uid uint32) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() || st.Uid != uid || st.Nlink != 1 || fi.Size() > 100 {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(f, 101))
	if err != nil {
		return false
	}
	return telegramPauseRE.MatchString(strings.TrimSuffix(string(b), "\n")) && strings.Count(string(b), "\n") == 1
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
