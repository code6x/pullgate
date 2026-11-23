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

// Receipts is the path of the directory of receipts: the listener writes
// one for each outbox file it handled (DESIGN.md section 8).
func (d Dirs) Receipts() string { return filepath.Join(d.inst(), "receipts") }

// Receipt contents. A receipt holds nothing from the message.
const (
	ReceiptSent   = "sent\n"   // Telegram took the message
	ReceiptFailed = "failed\n" // the file was unreadable, or Telegram refused it, and it was removed
)

// MaxReceipt is the largest receipt file root reads.
const MaxReceipt = 16

// MaxReceipts is the most receipts root reads in one call.
const MaxReceipts = 1000

// FixMode sets the owner (uid, gid; -1 keeps one) and the mode of an open
// file or directory. Tests replace it to play a kernel that does not keep
// a mode (issue #19): every caller reads the result back.
var FixMode = func(f *os.File, uid, gid int, mode os.FileMode) error {
	if uid >= 0 || gid >= 0 {
		if err := f.Chown(uid, gid); err != nil {
			return err
		}
	}
	return f.Chmod(mode)
}

// ErrNotKept is the error of a read-back that found another owner or mode
// than the one pullgate set.
var ErrNotKept = errors.New("the kernel did not keep the owner or the mode that pullgate set")

// readBack checks that the open file f has the owner uid, the group gid
// (-1: any) and exactly the mode bits mode (also setuid, setgid, sticky).
func readBack(f *os.File, what string, uid, gid int, mode os.FileMode) error {
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("read back %s: %w", what, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("read back %s: no owner. This is a bug in pullgate", what)
	}
	got := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	if (uid >= 0 && int(st.Uid) != uid) || (gid >= 0 && int(st.Gid) != gid) || got != mode {
		return fmt.Errorf("%w on %s: want owner %d, group %d, mode %s; got owner %d, group %d, mode %s. "+
			"Fix: run \"pullgate doctor\" as root; check that the unit keeps CAP_CHOWN and CAP_FOWNER, and the mount options of the file system",
			ErrNotKept, what, uid, gid, mode, st.Uid, st.Gid, got)
	}
	return nil
}

// SelfName is the spool name of the updates of pullgate itself (DESIGN.md
// section 15): Base/_update/{outbox,inbox,check}. An instance name matches
// [a-z0-9][a-z0-9-]{0,31}, so no instance can have this name.
const SelfName = "_update"

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
// directory, never through a path. No directory gets the setgid bit:
// pullgate-run@.service has RestrictSUIDSGID=yes, which refuses a chmod
// that sets it. Send gives each outbox file its group itself.
func (d Dirs) Ensure(uid uint32, o Owners) error {
	for _, p := range []string{d.Base, d.inst()} {
		if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("make %s: %w", p, err)
		}
	}
	for _, p := range []string{d.Base, d.inst()} {
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0) //nolint:gosec // the spool base and instance directory of root
		if err != nil {
			return fmt.Errorf("open %s: %w", p, err)
		}
		err = FixMode(f, -1, -1, 0o755)
		if err == nil {
			err = readBack(f, p, int(uid), -1, 0o755)
		}
		_ = f.Close()
		if err != nil {
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
		{"outbox", int(uid), o.ListenerGID, 0o770},
		{"inbox", o.ListenerUID, o.ListenerGID, 0o700},
		{"check", o.ListenerUID, o.ListenerGID, 0o700},
		{"receipts", o.ListenerUID, o.ListenerGID, 0o700},
	} {
		if err := root.Mkdir(s.name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("make %s: %w", s.name, err)
		}
		f, err := root.OpenFile(s.name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open %s/%s: %w", d.inst(), s.name, err)
		}
		err = FixMode(f, s.uid, s.gid, s.mode)
		if err == nil {
			err = readBack(f, d.inst()+"/"+s.name, s.uid, s.gid, s.mode)
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
// final (a new sequence name when final is ""). With gid >= 0 the file gets
// that group before it is renamed, so a reader never sees it with another.
// The owner and the mode are read back before the rename. It returns the
// final name.
func writeIn(dir string, data []byte, mode os.FileMode, gid int, final string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	tmp := randName()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", fmt.Errorf("write in %s: %w", dir, err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = FixMode(f, -1, gid, mode)
	}
	if err == nil {
		err = readBack(f, "a new file in "+dir, os.Geteuid(), gid, mode)
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
		return "", fmt.Errorf("write in %s: %w", dir, err)
	}
	return final, nil
}

// Send writes a message to the outbox (root). The file is owned by root,
// with group gid (the group of the listener user, Owners.ListenerGID) and
// mode 0640, so the listener can read it (DESIGN.md section 4). It returns
// the name of the file: the receipt of the listener has the same name.
func (d Dirs) Send(gid int, m Message) (string, error) {
	return d.SendAs(gid, NewMessageName(), m)
}

// NewMessageName is a new outbox file name, for SendAs.
func NewMessageName() string { return seqName() }

// SendAs is Send with the file name of NewMessageName, so that root can
// save the name with a question before the listener can see the file.
func (d Dirs) SendAs(gid int, name string, m Message) (string, error) {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsRune(name, '/') {
		return "", errors.New("write in the outbox: not a message name. This is a bug in pullgate")
	}
	if gid < 0 {
		return "", errors.New("write in the outbox: no group for the listener. This is a bug in pullgate")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return writeIn(d.Outbox(), b, 0o640, gid, name)
}

// DoctorPrefix starts the name of the test file that pullgate doctor
// writes to the outbox. The name starts with ".", so the listener never
// sends it.
const DoctorPrefix = ".doctor-"

// WriteDoctorTest writes a small file to the outbox as Send does, under a
// name that the listener skips, and returns its path. The caller removes
// it with RemoveDoctorTest.
func (d Dirs) WriteDoctorTest(gid int) (string, error) {
	if gid < 0 {
		return "", errors.New("write in the outbox: no group for the listener. This is a bug in pullgate")
	}
	n, err := writeIn(d.Outbox(), []byte("{}"), 0o640, gid, DoctorPrefix+randName()[5:])
	if err != nil {
		return "", err
	}
	return filepath.Join(d.Outbox(), n), nil
}

// RemoveDoctorTest removes a file of WriteDoctorTest.
func (d Dirs) RemoveDoctorTest(path string) error {
	n := filepath.Base(path)
	if filepath.Dir(path) != d.Outbox() || !strings.HasPrefix(n, DoctorPrefix) {
		return errors.New("not a doctor test file. This is a bug in pullgate")
	}
	root, err := os.OpenRoot(d.Outbox())
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.Remove(n)
}

// WriteReceipt writes the receipt of the outbox file name (the listener):
// ReceiptSent when sent is true, else ReceiptFailed.
func (d Dirs) WriteReceipt(name string, sent bool) error {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsRune(name, '/') {
		return errors.New("not an outbox file name. This is a bug in pullgate")
	}
	c := ReceiptFailed
	if sent {
		c = ReceiptSent
	}
	_, err := writeIn(d.Receipts(), []byte(c), 0o600, -1, name)
	return err
}

// TakeReceipts reads and removes the receipts (root): the map holds true
// for a sent message and false for a failed one, by outbox file name. Each
// file must be a regular file owned by the listener user, with one of the
// two contents; a bad file is removed and comes back as an error. A
// missing directory (made by an older version) is no receipt.
func (d Dirs) TakeReceipts(listenerUID int) (map[string]bool, []error, error) {
	root, err := os.OpenRoot(d.Receipts())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()
	// At most MaxReceipts names are read; the rest waits for the next run.
	dir, err := root.Open(".")
	if err != nil {
		return nil, nil, err
	}
	all, err := dir.Readdirnames(MaxReceipts)
	_ = dir.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	var names []string
	for _, n := range all {
		if !strings.HasPrefix(n, ".") {
			names = append(names, n)
			continue
		}
		// No dot file belongs here but a fresh .tmp- file of the listener:
		// removed, so they cannot fill the batch for good.
		if fi, err := root.Lstat(n); err == nil && (!strings.HasPrefix(n, ".tmp-") || time.Since(fi.ModTime()) > time.Minute) {
			_ = root.Remove(n)
		}
	}
	out := map[string]bool{}
	var bad []error
	for _, n := range names {
		b, err := readOne(root, n, MaxReceipt, listenerUID)
		if rerr := root.Remove(n); rerr != nil && err == nil {
			err = rerr
		}
		switch {
		case err != nil:
		case string(b) == ReceiptSent:
			out[n] = true
			continue
		case string(b) == ReceiptFailed:
			out[n] = false
			continue
		default:
			err = errors.New("not the content of a receipt")
		}
		bad = append(bad, fmt.Errorf("receipt %s: %w", n, err))
	}
	return out, bad, nil
}

// PeekReceipt reads the receipt of the outbox file name and leaves it in
// place (for pullgate status). found is false when there is none or it is
// bad.
func (d Dirs) PeekReceipt(listenerUID int, name string) (sent, found bool) {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsRune(name, '/') {
		return false, false
	}
	root, err := os.OpenRoot(d.Receipts())
	if err != nil {
		return false, false
	}
	defer func() { _ = root.Close() }()
	b, err := readOne(root, name, MaxReceipt, listenerUID)
	switch {
	case err != nil:
		return false, false
	case string(b) == ReceiptSent:
		return true, true
	case string(b) == ReceiptFailed:
		return false, true
	}
	return false, false
}

// OutboxHas reports whether the outbox still holds the file name: the
// listener has not handled it yet.
func (d Dirs) OutboxHas(name string) bool {
	root, err := os.OpenRoot(d.Outbox())
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	_, err = root.Lstat(name)
	return err == nil
}

// WriteApproval writes an approval to the inbox (the listener).
func (d Dirs) WriteApproval(a Approval) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = writeIn(d.Inbox(), b, 0o600, -1, "")
	return err
}

// WriteCheck writes a check request (the listener). It always has the same
// name, so many requests before root reads them are one file.
func (d Dirs) WriteCheck() error {
	_, err := writeIn(d.CheckDir(), []byte(CheckContent), 0o600, -1, CheckFile)
	return err
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

// Request kinds besides the check request (code6x/pullgate#32). Each has
// one file name, and its content is the name and a newline: no data from
// the message.
const (
	RequestStatus   = "status"
	RequestPause    = "pause"
	RequestResume   = "resume"    // code6x/pullgate#33
	RequestAskAgain = "ask-again" // code6x/pullgate#33
)

// WriteRequest writes a status or pause request (the listener).
func (d Dirs) WriteRequest(kind string) error {
	if kind != RequestStatus && kind != RequestPause && kind != RequestResume && kind != RequestAskAgain {
		return fmt.Errorf("unknown request kind %q", kind)
	}
	_, err := writeIn(d.CheckDir(), []byte(kind+"\n"), 0o600, -1, kind)
	return err
}

// Requests counts the valid requests that TakeRequests took.
type Requests struct{ Check, Status, Pause, Resume, AskAgain int }

// Any reports whether there was a valid request.
func (r Requests) Any() bool { return r.Check+r.Status+r.Pause+r.Resume+r.AskAgain > 0 }

// TakeChecks is TakeRequests, for callers that know only check requests.
func (d Dirs) TakeChecks(listenerUID int) (int, []error, error) {
	r, bad, err := d.TakeRequests(listenerUID)
	return r.Check, bad, err
}

// TakeRequests reads and removes every file in the check directory (root).
// It returns the valid requests, and an error for each bad file. A valid
// request is a regular file owned by the listener user: CheckFile with
// exactly CheckContent, or a file named RequestStatus or RequestPause with
// exactly its name and a newline.
func (d Dirs) TakeRequests(listenerUID int) (Requests, []error, error) {
	var r Requests
	root, err := os.OpenRoot(d.CheckDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return r, nil, nil
		}
		return r, nil, fmt.Errorf("open the check directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	names, err := list(root)
	if err != nil {
		return r, nil, fmt.Errorf("read the check directory: %w", err)
	}
	var bad []error
	for _, name := range names {
		b, err := readOne(root, name, MaxCheck, listenerUID)
		if rerr := root.RemoveAll(name); rerr != nil {
			return r, bad, fmt.Errorf("remove %s/%s: %w. Fix: remove it as root", d.CheckDir(), name, rerr)
		}
		var count *int
		want := ""
		switch name {
		case CheckFile:
			count, want = &r.Check, CheckContent
		case RequestStatus:
			count, want = &r.Status, RequestStatus+"\n"
		case RequestPause:
			count, want = &r.Pause, RequestPause+"\n"
		case RequestResume:
			count, want = &r.Resume, RequestResume+"\n"
		case RequestAskAgain:
			count, want = &r.AskAgain, RequestAskAgain+"\n"
		}
		switch {
		case err != nil:
		case count == nil:
			err = errors.New("not the name of a request")
		case string(b) != want:
			err = errors.New("not the content of a request")
		}
		if err != nil {
			bad = append(bad, fmt.Errorf("check file %s: %w", name, err))
			continue
		}
		*count++
	}
	return r, bad, nil
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
