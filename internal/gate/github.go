package gate

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/code6x/pullgate/internal/ghdeploy"
	"github.com/code6x/pullgate/internal/state"
)

// ghRun is the GitHub queue of this process (DESIGN.md section 22). It is
// open only while the instance lock is held.
type ghRun struct {
	q     *ghdeploy.Queue
	spent time.Duration // the time this process spent sending so far
}

// ghOn reports whether this process reports to GitHub.
func (e *Env) ghOn() bool { return e.gh != nil }

// ghOpen opens the queue. The caller holds the instance lock. A problem
// is logged and turns the reports off for this process: it never stops a
// deploy.
func (e *Env) ghOpen() {
	if e.Cfg.GitHub == nil || e.gh != nil {
		return
	}
	if strings.HasPrefix(e.Cfg.GitHub.API(), "http://") {
		e.Log.Warn("github: api_url is http, not https: use it only for a test server on 127.0.0.1", "api_url", e.Cfg.GitHub.API())
	}
	q, err := ghdeploy.Open(ghdeploy.Dir(e.Dir.Path), e.UID)
	if err != nil {
		e.Log.Error("github: the queue cannot be used; no GitHub reports in this run. The deploy goes on", "err", err)
		return
	}
	e.gh = &ghRun{q: q}
}

func (e *Env) ghSave() {
	if err := e.gh.q.Save(); err != nil {
		e.Log.Error("github: save the queue; the deploy goes on", "err", err)
	}
}

// ghStart queues a new deployment for sha and its in_progress status,
// and saves the queue. It returns the key (0 when the reports are off).
func (e *Env) ghStart(role, sha string) int64 {
	if !e.ghOn() {
		return 0
	}
	desc := ghdeploy.DescApplyStarted
	if role == ghdeploy.RoleRollback {
		desc = ghdeploy.DescRollbackStarted
	}
	k := e.gh.q.Start(role, sha, e.ghTarget(), desc, e.now())
	e.ghSave()
	return k
}

func (e *Env) ghTarget() ghdeploy.Target {
	g := e.Cfg.GitHub
	return ghdeploy.Target{API: g.API(), Repository: g.Repository, Environment: g.Environment}
}

// ghStatus queues a status on the deployment key and saves the queue.
func (e *Env) ghStatus(key int64, st, desc string) {
	if !e.ghOn() || key == 0 {
		return
	}
	e.gh.q.Status(key, st, desc, e.now())
	e.ghSave()
}

// ghStepWindow is the most time between the queue write and the state
// save of one step.
const ghStepWindow = 10 * time.Minute

// ghStateRole is the role and the SHA of the deployment of an apply in
// progress: phase apply is the deployment of SHA, phase rollback the one
// of OldSHA.
func ghStateRole(ip *state.InProgress) (string, string) {
	if ip.Phase == "rollback" {
		return ghdeploy.RoleRollback, ip.OldSHA
	}
	return ghdeploy.RoleApply, ip.SHA
}

// ghCheck runs after the lock is taken, before any exit of run and apply.
// An open deployment that is not the one of the state's apply in progress
// belongs to a step whose state save never happened: its command did not
// run, so it gets "stopped before the step started", and nothing pauses.
func (e *Env) ghCheck(st *state.State) {
	if !e.ghOn() {
		return
	}
	var keep *ghdeploy.Deployment
	if ip := st.InProgress; ip != nil {
		role, sha := ghStateRole(ip)
		keep = e.gh.q.Newest(role, sha, true)
	}
	changed := false
	for _, d := range e.gh.q.OpenDeployments() {
		if d != keep {
			e.Log.Warn("github: a deployment of a step that never started; posting error", "sha", d.SHA, "role", d.Role)
			e.gh.q.Status(d.Key, ghdeploy.Error, ghdeploy.DescStopped, e.now())
			changed = true
		}
	}
	if changed {
		e.ghSave()
	}
}

// ghCutOff queues "cut off" on the deployment of the apply in progress
// ip, after anything queued for it already. A deployment whose last
// status is error already (a failed save of the broken state) gets no
// second one.
func (e *Env) ghCutOff(ip *state.InProgress) {
	if !e.ghOn() {
		return
	}
	role, sha := ghStateRole(ip)
	d := e.gh.q.Newest(role, sha, false)
	// Only the deployment of this step: queued just before or after the
	// state save of in_progress, never an older one of the same commit.
	if d == nil || d.Dropped || d.Final == ghdeploy.Error || d.Queued.Sub(ip.Started).Abs() > ghStepWindow {
		return
	}
	e.gh.q.Status(d.Key, ghdeploy.Error, ghdeploy.DescCutOff, e.now())
	e.ghSave()
}

// ghSend sends what waits in the queue, for at most the send limit of
// this process in total. It never fails the step.
func (e *Env) ghSend() {
	if !e.ghOn() || len(e.gh.q.Posts) == 0 {
		return
	}
	limit := e.GitHubSendLimit
	if limit <= 0 {
		limit = ghdeploy.SendLimit
	}
	// The limit counts the time spent sending, not the time of the apply
	// command between two sends.
	left := limit - e.gh.spent
	if left <= 0 {
		e.Log.Info("github: the send time of this process is used up; the rest waits for the next run", "waiting", len(e.gh.q.Posts))
		return
	}
	start := time.Now()
	defer func() { e.gh.spent += time.Since(start) }()
	h := e.GitHubHTTP
	if h == nil {
		h = ghdeploy.NewHTTPClient(nil)
	}
	g := e.Cfg.GitHub
	s := &ghdeploy.Sender{
		Q: e.gh.q, Log: e.Log, Now: e.now, Deadline: start.Add(left), Instance: e.Cfg.Name,
		C:       &ghdeploy.Client{API: g.API(), Repository: g.Repository, Environment: g.Environment, Token: e.ghToken, HTTP: h},
		Refused: func(text string) { e.notice("⚠ " + text + ".") },
	}
	// Not the context of the process: a stop (SIGTERM) still sends, as
	// the time limit allows.
	s.Send(context.Background())
}

// ghDone sends what waits, at the end of a run, an apply or a status
// --resume, and closes the queue of this process.
func (e *Env) ghDone() {
	e.ghSend()
	e.gh = nil
}

// ghStatusLine is the GitHub line of pullgate status and /status: fixed
// text only, never GitHub's answer and never the token.
func (e *Env) ghStatusLine() string {
	g := e.Cfg.GitHub
	if g == nil {
		return ""
	}
	var q *ghdeploy.Queue
	if e.gh != nil {
		q = e.gh.q
	} else {
		var err error
		if q, err = ghdeploy.Read(ghdeploy.Dir(e.Dir.Path), e.UID); err != nil {
			return "the queue cannot be read. Fix: run pullgate doctor " + e.Cfg.Name + " as root"
		}
	}
	last := "no post yet"
	if l := q.Last; l != nil {
		res := "OK"
		if !l.OK {
			res = l.Error
		}
		last = fmt.Sprintf("last post %s: %s, %s", l.At.UTC().Format("2006-01-02 15:04 MST"), l.What, res)
	}
	return fmt.Sprintf("%s; %d posts wait (environment %s)", last, len(q.Posts), g.Environment)
}

// GitHubProbe is the check of pullgate doctor --github: GET of the repo
// and of one deployment, with the token. It writes nothing to GitHub.
func (e *Env) GitHubProbe(ctx context.Context, h *http.Client) error {
	g := e.Cfg.GitHub
	if g == nil {
		return nil
	}
	if h == nil {
		h = ghdeploy.NewHTTPClient(nil)
	}
	c := &ghdeploy.Client{API: g.API(), Repository: g.Repository, Environment: g.Environment, Token: e.ghToken, HTTP: h}
	for _, p := range []string{"", "/deployments?per_page=1"} {
		cctx, cancel := context.WithTimeout(ctx, ghdeploy.CallTimeout)
		code, err := c.Get(cctx, p)
		cancel()
		what := "GET /repos/" + g.Repository + p
		switch {
		case err != nil:
			return fmt.Errorf("%s: %w. Fix: check the network of the host and api_url in [github]", what, err)
		case code == http.StatusOK:
		case code >= 300 && code < 400:
			return fmt.Errorf("%s: HTTP %d: the repo moved. Fix: set repository in [github] to its new name", what, code)
		case code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotFound:
			return fmt.Errorf("%s: HTTP %d: the token is refused, or cannot see the repo. Fix: make a new fine-grained token for %s with Deployments: read and write, and write it to %s", what, code, g.Repository, g.TokenFile)
		default:
			return fmt.Errorf("%s: HTTP %d. Fix: check api_url in [github], then try again", what, code)
		}
	}
	return nil
}
