// Package spool holds the files between root and the listener (DESIGN.md
// section 8): the outbox (root to listener), the inbox (listener to root)
// and the check directory (check requests, listener to root). Root never follows a path that the listener can change: it works
// through os.Root, and creates files with O_EXCL|O_NOFOLLOW.
package spool

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/code6x/pullgate/internal/safedir"
)

// Message kinds of the outbox.
const (
	KindAsk    = "ask"
	KindEdit   = "edit"
	KindNotice = "notice"
	KindReply  = "reply"
)

// Message is one outbox file.
type Message struct {
	Kind     string    `json:"kind"`
	Question int64     `json:"question,omitempty"`
	Code     string    `json:"code,omitempty"`
	Expires  time.Time `json:"expires,omitzero"`
	Text     string    `json:"text"`
}

// Approval is one inbox file.
type Approval struct {
	Code   string `json:"code"`
	Action string `json:"action"` // "apply" or "skip"
}

// MaxApproval is the largest inbox file.
const MaxApproval = 1024

// MaxMessage is the largest outbox file.
const MaxMessage = 64 << 10

// Owners are the users of the spool.
type Owners struct {
	ListenerUID int // owns the inbox
	ListenerGID int // the group of the outbox
}

// Dirs is the spool of one instance: Base/NAME/{outbox,inbox}.
type Dirs struct {
	Base string // /var/spool/pullgate
	Name string
}

func (d Dirs) inst() string { return filepath.Join(d.Base, d.Name) }

// Outbox is the path of the outbox.
func (d Dirs) Outbox() string { return filepath.Join(d.inst(), "outbox") }

// Inbox is the path of the inbox.
func (d Dirs) Inbox() string { return filepath.Join(d.inst(), "inbox") }

// CheckDir is the path of the directory of check requests.
func (d Dirs) CheckDir() string { return filepath.Join(d.inst(), "check") }

// CheckFile is the one name a check request has in the check directory.
const CheckFile = "request"

// CheckContent is the whole content of a check request. It holds no data
// from the message (DESIGN.md section 8).
const CheckContent = "check\n"

// MaxCheck is the largest check request file root reads.
const MaxCheck = 64

// Ensure makes the spool of the instance with the owners and modes of
// DESIGN.md section 4. Base and Base/NAME belong to root (uid) and must
// pass the check of rule 6; outbox and inbox are fixed through an open
// directory, never through a path.
func (d Dirs) Ensure(uid uint32, o Owners) error {
	for _, p := range []string{d.Base, d.inst()} {
		if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("make %s: %w", p, err)
		}
	}
	for _, p := range []string{d.Base, d.inst()} {
		if err := os.Chmod(p, 0o755); err != nil {
			return fmt.Errorf("fix %s: %w", p, err)
		}
	}
	if err := safedir.CheckParents(d.inst(), uid); err != nil {
		return fmt.Errorf("the spool is not safe: %w", err)
	}
	root, err := os.OpenRoot(d.inst())
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, s := range []struct {
		name     string
		uid, gid int
		mode     os.FileMode
	}{
		{"outbox", int(uid), o.ListenerGID, 0o770 | os.ModeSetgid},
		{"inbox", o.ListenerUID, o.ListenerGID, 0o700},
		{"check", o.ListenerUID, o.ListenerGID, 0o700},
	} {
		if err := root.Mkdir(s.name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("make %s: %w", s.name, err)
		}
		f, err := root.OpenFile(s.name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open %s/%s: %w", d.inst(), s.name, err)
		}
		err = f.Chown(s.uid, s.gid)
		if err == nil {
			err = f.Chmod(s.mode)
		}
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("fix %s/%s: %w", d.inst(), s.name, err)
		}
	}
	return nil
}

func randName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return ".tmp-" + hex.EncodeToString(b)
}

// seqName is a name that sorts by time.
func seqName() string {
	return fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), randName()[5:])
}

// writeIn writes data as a new file in dir, through os.Root: a random
// temporary name with O_EXCL|O_NOFOLLOW, then a rename in the same dir to
// final (a new sequence name when final is "").
func writeIn(dir string, data []byte, mode os.FileMode, final string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	tmp := randName()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("write in %s: %w", dir, err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if final == "" {
		final = seqName()
	}
	if err == nil {
		err = root.Rename(tmp, final)
	}
	if err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("write in %s: %w", dir, err)
	}
	return nil
}

// Send writes a message to the outbox (root).
func (d Dirs) Send(m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeIn(d.Outbox(), b, 0o640, "")
}

// WriteApproval writes an approval to the inbox (the listener).
func (d Dirs) WriteApproval(a Approval) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return writeIn(d.Inbox(), b, 0o600, "")
}

// WriteCheck writes a check request (the listener). It always has the same
// name, so many requests before root reads them are one file.
func (d Dirs) WriteCheck() error {
	return writeIn(d.CheckDir(), []byte(CheckContent), 0o600, CheckFile)
}

// HasCheck reports whether the check directory holds a file whose name
// does not start with ".". It reads and removes nothing. It opens the
// instance directory first and "check" inside it, so it never follows a
// symlink in place of "check".
func (d Dirs) HasCheck() bool {
	inst, err := os.OpenRoot(d.inst())
	if err != nil {
		return false
	}
	defer func() { _ = inst.Close() }()
	root, err := inst.OpenRoot("check")
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(".")
	if err != nil {
		return false
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return false
	}
	for _, n := range names {
		if !strings.HasPrefix(n, ".") {
			return true
		}
	}
	return false
}

// TakeChecks reads and removes every file in the check directory (root).
// It returns how many were valid check requests, and an error for each bad
// file. A valid request is a regular file named CheckFile, owned by the
// listener user, with exactly CheckContent in it.
func (d Dirs) TakeChecks(listenerUID int) (int, []error, error) {
	root, err := os.OpenRoot(d.CheckDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil, nil
		}
		return 0, nil, fmt.Errorf("open the check directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	names, err := list(root)
	if err != nil {
		return 0, nil, fmt.Errorf("read the check directory: %w", err)
	}
	n := 0
	var bad []error
	for _, name := range names {
		b, err := readOne(root, name, MaxCheck, listenerUID)
		if rerr := root.RemoveAll(name); rerr != nil {
			return n, bad, fmt.Errorf("remove %s/%s: %w. Fix: remove it as root", d.CheckDir(), name, rerr)
		}
		switch {
		case err != nil:
		case name != CheckFile:
			err = errors.New("not the name of a check request")
		case string(b) != CheckContent:
			err = errors.New("not the content of a check request")
		}
		if err != nil {
			bad = append(bad, fmt.Errorf("check file %s: %w", name, err))
			continue
		}
		n++
	}
	return n, bad, nil
}

// list returns the names in dir, sorted, without names that start with ".".
// It removes temporary files older than a minute.
func list(root *os.Root) ([]string, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, ".tmp-") {
			if fi, err := root.Lstat(n); err == nil && time.Since(fi.ModTime()) > 10*time.Second {
				_ = root.Remove(n)
			}
			continue
		}
		if strings.HasPrefix(n, ".") {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// readOne reads a regular file of at most max bytes through root, with
// O_NOFOLLOW. With uid >= 0 the file must be owned by uid.
func readOne(root *os.Root, name string, max int64, uid int) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); uid >= 0 && (!ok || int(st.Uid) != uid) {
		return nil, errors.New("not owned by the listener user")
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("larger than %d bytes", max)
	}
	return io.ReadAll(io.LimitReader(f, max+1))
}

// Entry is one approval read from the inbox, or the error of a bad file.
type Entry struct {
	Name     string
	Approval Approval
	Err      error
}

// TakeApprovals reads the approvals in the inbox in name order, at most max
// of them (0: all). Each file is removed before it is returned, so it is
// used once. A bad file is removed and comes back with Err set.
func (d Dirs) TakeApprovals(listenerUID int, max int) ([]Entry, error) {
	root, err := os.OpenRoot(d.Inbox())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open the inbox: %w", err)
	}
	defer func() { _ = root.Close() }()
	names, err := list(root)
	if err != nil {
		return nil, fmt.Errorf("read the inbox: %w", err)
	}
	var out []Entry
	for _, n := range names {
		if max > 0 && len(out) >= max {
			break
		}
		e := Entry{Name: n}
		b, err := readOne(root, n, MaxApproval, listenerUID)
		if rerr := root.RemoveAll(n); rerr != nil {
			// It cannot be used, and it stays: stop, so nobody loops on it.
			return out, fmt.Errorf("remove %s/%s: %w. Fix: remove it as root", d.Inbox(), n, rerr)
		}
		if err == nil {
			e.Approval, err = ParseApproval(b)
		}
		e.Err = err
		out = append(out, e)
	}
	return out, nil
}

// ParseApproval parses an approval strictly.
func ParseApproval(b []byte) (Approval, error) {
	var a Approval
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return Approval{}, fmt.Errorf("bad approval: %w", err)
	}
	if dec.More() {
		return Approval{}, errors.New("bad approval: data after the object")
	}
	if a.Action != "apply" && a.Action != "skip" {
		return Approval{}, errors.New("bad approval: unknown action")
	}
	if len(a.Code) != 22 {
		return Approval{}, errors.New("bad approval: bad code")
	}
	return a, nil
}

// OutboxFile is one message read by the listener.
type OutboxFile struct {
	Name    string
	Message Message
	Err     error
	Age     time.Duration
}

// ReadOutbox reads the outbox in name order (the listener). It does not
// remove the files; call Done when a message is handled.
func (d Dirs) ReadOutbox() ([]OutboxFile, error) {
	root, err := os.OpenRoot(d.Outbox())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = root.Close() }()
	names, err := list(root)
	if err != nil {
		return nil, err
	}
	var out []OutboxFile
	for _, n := range names {
		of := OutboxFile{Name: n}
		if ts, _, ok := strings.Cut(n, "-"); ok {
			if ns, err := strconv.ParseInt(ts, 10, 64); err == nil {
				of.Age = time.Since(time.Unix(0, ns))
			}
		}
		b, err := readOne(root, n, MaxMessage, -1)
		if err == nil {
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			err = dec.Decode(&of.Message)
		}
		of.Err = err
		out = append(out, of)
	}
	return out, nil
}

// Done removes a handled outbox file.
func (d Dirs) Done(name string) error {
	root, err := os.OpenRoot(d.Outbox())
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.Remove(name)
}
