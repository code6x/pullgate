// Package ghdeploy posts what root did to GitHub as deployments and
// deployment statuses (DESIGN.md section 22). A post is only a report: no
// error here ever changes an apply. Root writes each post to a queue in
// the state folder before the state save of its step, and sends the queue
// later, in order, under the instance lock.
package ghdeploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/code6x/pullgate/internal/state"
)

// The roles of a deployment: the apply of a new commit, or the rollback
// to the commit that was applied before.
const (
	RoleApply    = "apply"
	RoleRollback = "rollback"
)

// The deployment states that pullgate posts.
const (
	InProgress = "in_progress"
	Success    = "success"
	Failure    = "failure"
	Error      = "error"
)

// The descriptions of the statuses. They are fixed text: never output of
// a command, never a host name. Each is at most 140 characters.
const (
	DescApplyStarted     = "the apply started"
	DescApplied          = "applied: the apply and the health check passed"
	DescFixedForward     = "applied: the fix forward passed; the instance is no longer broken"
	DescRollbackStarts   = "failed: the rollback to the commit before starts"
	DescRollbackStarted  = "the rollback to this commit started"
	DescRolledBack       = "rolled back: this commit is live again"
	DescBroken           = "broken: the rollback failed; the host may be in a half state"
	DescHeld             = "held: the host may be in a half state"
	DescFixForwardFailed = "broken: the fix forward failed"
	DescCutOff           = "cut off: check the host"
	DescStopped          = "stopped before the step started"
)

// Descriptions are all the descriptions, for the tests.
var Descriptions = []string{
	DescApplyStarted, DescApplied, DescFixedForward, DescRollbackStarts, DescRollbackStarted,
	DescRolledBack, DescBroken, DescHeld, DescFixForwardFailed, DescCutOff, DescStopped,
}

// MaxAge is how long a post is tried again; then it is dropped.
const MaxAge = 24 * time.Hour

// maxQueue is the largest queue file that root reads.
const maxQueue = 1 << 20

// maxPosts is the most posts that wait; a post is at most a few hundred
// bytes, so the file stays far below maxQueue.
const maxPosts = 1000

// keepClosed is how many closed deployments the queue keeps, so that a
// cut-off finds the deployment of its step after a failed state save.
const keepClosed = 20

// Deployment is one deployment: a local key, and the GitHub id once its
// create was sent.
type Deployment struct {
	Key  int64  `json:"key"`
	Role string `json:"role"`
	SHA  string `json:"sha"`
	ID   int64  `json:"id,omitempty"`
	// Environment is the environment when the deployment was queued, so a
	// change of the config does not move a queued deployment.
	Environment string `json:"environment"`
	// Final is the last final status (success, failure or error) that was
	// queued; "" while the deployment is open.
	Final   string    `json:"final,omitempty"`
	Dropped bool      `json:"dropped,omitempty"`
	Queued  time.Time `json:"queued"`
}

// Open reports whether the deployment was created (queued or sent) and
// has no final status queued or sent.
func (d *Deployment) Open() bool { return d.Final == "" && !d.Dropped }

// Post is one GitHub API call that waits: the create of a deployment, or
// one status on it.
type Post struct {
	Key         int64     `json:"key"`
	Create      bool      `json:"create,omitempty"`
	State       string    `json:"state,omitempty"`
	Description string    `json:"description,omitempty"`
	Queued      time.Time `json:"queued"`
}

// What is the post for a log line or pullgate status: "create" or the
// state.
func (p *Post) What() string {
	if p.Create {
		return "create"
	}
	return p.State
}

// Last is the last post that was tried.
type Last struct {
	At    time.Time `json:"at"`
	What  string    `json:"what"`
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"` // the HTTP code and a fixed reason, never GitHub's text
}

// Queue is the content of the queue file.
type Queue struct {
	Version     int           `json:"version"`
	NextKey     int64         `json:"next_key"`
	Deployments []*Deployment `json:"deployments"`
	Posts       []*Post       `json:"posts"`
	Last        *Last         `json:"last,omitempty"`
	RefusedNote time.Time     `json:"refused_note,omitzero"` // the last token-refused notice

	path string
}

// Dir is the queue folder of the instance state folder stateDir.
func Dir(stateDir string) string { return filepath.Join(stateDir, "github") }

// File is the queue file in the queue folder dir.
func File(dir string) string { return filepath.Join(dir, "queue.json") }

// Open makes the queue folder (owner uid, mode 0700, read back) and reads
// the queue. A missing file is an empty queue.
func Open(dir string, uid uint32) (*Queue, error) {
	if err := ensureDir(dir, uid); err != nil {
		return nil, err
	}
	return Read(dir, uid)
}

// Read reads the queue without making the folder; a missing folder or
// file is an empty queue.
func Read(dir string, uid uint32) (*Queue, error) {
	q := &Queue{Version: 1, NextKey: 1, path: File(dir)}
	b, err := readFile(q.path, uid, maxQueue)
	if errors.Is(err, fs.ErrNotExist) {
		return q, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the GitHub queue %s: %w", q.path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(q); err != nil || q.Version != 1 || q.NextKey < 1 {
		return nil, fmt.Errorf("the GitHub queue %s is broken. Fix: remove the file; its reports are lost, the deploys are not touched", q.path)
	}
	for _, d := range q.Deployments {
		if d == nil {
			return nil, fmt.Errorf("the GitHub queue %s is broken. Fix: remove the file; its reports are lost, the deploys are not touched", q.path)
		}
	}
	for _, p := range q.Posts {
		if p == nil || q.find(p.Key) == nil {
			return nil, fmt.Errorf("the GitHub queue %s is broken. Fix: remove the file; its reports are lost, the deploys are not touched", q.path)
		}
	}
	return q, nil
}

// Save writes the queue atomically, mode 0600, and syncs it.
func (q *Queue) Save() error {
	b, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(q.path, append(b, '\n'), 0o600)
}

func (q *Queue) find(key int64) *Deployment {
	for _, d := range q.Deployments {
		if d.Key == key {
			return d
		}
	}
	return nil
}

// Start queues the create of a deployment for sha, and its in_progress
// status. It returns the key of the deployment.
func (q *Queue) Start(role, sha, env, desc string, now time.Time) int64 {
	// A bounded queue: a long outage drops the oldest posts first, so the
	// file always stays below the size that Read takes.
	for len(q.Posts) >= maxPosts {
		q.drop(q.find(q.Posts[0].Key))
	}
	k := q.NextKey
	q.NextKey++
	q.Deployments = append(q.Deployments, &Deployment{Key: k, Role: role, SHA: sha, Environment: env, Queued: now})
	q.Posts = append(q.Posts, &Post{Key: k, Create: true, Queued: now}, &Post{Key: k, State: InProgress, Description: desc, Queued: now})
	q.prune()
	return k
}

// Status queues a status on the deployment key. A key that is unknown
// or dropped queues nothing.
func (q *Queue) Status(key int64, st, desc string, now time.Time) {
	d := q.find(key)
	if d == nil || d.Dropped {
		return
	}
	if st != InProgress {
		d.Final = st
	}
	q.Posts = append(q.Posts, &Post{Key: key, State: st, Description: desc, Queued: now})
}

// OpenDeployments are the open deployments, oldest first.
func (q *Queue) OpenDeployments() []*Deployment {
	var out []*Deployment
	for _, d := range q.Deployments {
		if d.Open() {
			out = append(out, d)
		}
	}
	return out
}

// Newest is the newest deployment of role and sha, also a dropped one,
// or nil. open asks for an open one only.
func (q *Queue) Newest(role, sha string, open bool) *Deployment {
	for i := len(q.Deployments) - 1; i >= 0; i-- {
		d := q.Deployments[i]
		if d.Role == role && d.SHA == sha && (!open || d.Open()) {
			return d
		}
	}
	return nil
}

// drop removes the posts of d that wait, and marks it dropped.
func (q *Queue) drop(d *Deployment) {
	d.Dropped = true
	keep := q.Posts[:0]
	for _, p := range q.Posts {
		if p.Key != d.Key {
			keep = append(keep, p)
		}
	}
	q.Posts = keep
}

// prune forgets closed deployments with no post that waits, but the
// newest keepClosed of them.
func (q *Queue) prune() {
	waiting := map[int64]bool{}
	for _, p := range q.Posts {
		waiting[p.Key] = true
	}
	closed := 0
	for _, d := range q.Deployments {
		if !d.Open() && !waiting[d.Key] {
			closed++
		}
	}
	keep := q.Deployments[:0]
	for _, d := range q.Deployments {
		if !d.Open() && !waiting[d.Key] && closed > keepClosed {
			closed--
			continue
		}
		keep = append(keep, d)
	}
	q.Deployments = keep
}
