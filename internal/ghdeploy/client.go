package ghdeploy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
)

// maxAnswer is the most bytes of a GitHub answer that are read.
const maxAnswer = 64 << 10

// CallTimeout is the longest wait for one call.
const CallTimeout = 5 * time.Second

// SendLimit is the most time that one process spends on sending.
const SendLimit = 10 * time.Second

// NewHTTPClient is the HTTP client of the posts: TLS 1.2 or newer, no
// proxy, no redirect. roots nil is the system roots (tests give their own).
func NewHTTPClient(roots *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: CallTimeout,
		Transport: &http.Transport{
			Proxy:                  nil,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
			DialContext:            (&net.Dialer{Timeout: CallTimeout}).DialContext,
			TLSHandshakeTimeout:    CallTimeout,
			ResponseHeaderTimeout:  CallTimeout,
			MaxResponseHeaderBytes: 64 << 10,
			// No idle connections: the transport logs bytes that come on
			// an idle one, and those could echo the token.
			DisableKeepAlives: true,
		},
		// A renamed repo answers with a redirect: never follow it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Client talks to the GitHub API for one instance. Token is never
// printed, logged or returned in an error.
type Client struct {
	API         string // without a slash at the end
	Repository  string // owner/repo
	Environment string
	Token       string
	HTTP        *http.Client
}

// Kind says what to do with a post after a call.
type Kind int

// The kinds of answer.
const (
	Sent  Kind = iota // 201
	Retry             // try again later, in order
	Drop              // drop it, with the statuses that depend on it
)

// Answer is what one call gave.
type Answer struct {
	Kind    Kind
	Code    int    // the HTTP code; 0 when no answer came
	ID      int64  // the deployment id of a create
	Reason  string // the HTTP code and a fixed reason, never GitHub's text
	Refused bool   // the token is refused (401, 403 that is not a rate limit, 404 on a create)
}

type createBody struct {
	Ref                   string   `json:"ref"`
	Task                  string   `json:"task"`
	AutoMerge             bool     `json:"auto_merge"`
	RequiredContexts      []string `json:"required_contexts"`
	Environment           string   `json:"environment"`
	ProductionEnvironment bool     `json:"production_environment"`
	TransientEnvironment  bool     `json:"transient_environment"`
	Description           string   `json:"description"`
}

type statusBody struct {
	State       string `json:"state"`
	Description string `json:"description"`
}

func (c *Client) repoURL() string { return c.API + "/repos/" + c.Repository }

// Post sends one post. d is its deployment; a status needs d.ID.
func (c *Client) Post(ctx context.Context, d *Deployment, p *Post) Answer {
	var body any
	url := c.repoURL() + "/deployments"
	if p.Create {
		body = createBody{
			Ref: d.SHA, Task: "deploy", AutoMerge: false, RequiredContexts: []string{},
			Environment: d.Environment, Description: "pullgate " + d.Role,
		}
	} else {
		url += "/" + strconv.FormatInt(d.ID, 10) + "/statuses"
		body = statusBody{State: p.State, Description: p.Description}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Answer{Kind: Drop, Reason: "the post could not be made"}
	}
	code, hdr, ans, err := c.do(ctx, http.MethodPost, url, b)
	if errors.Is(err, errBody) && code == http.StatusCreated && p.Create {
		return Answer{Kind: Retry, Code: code, Reason: "HTTP 201: the answer was cut off (a network error or a timeout)"}
	}
	if err != nil && code == 0 {
		return Answer{Kind: Retry, Reason: "no answer from GitHub (a network error, a TLS error or a timeout)"}
	}
	a := Answer{Code: code}
	switch {
	case code == http.StatusCreated && !p.Create:
		a.Kind, a.Reason = Sent, ""
	case code == http.StatusCreated:
		var r struct {
			ID int64 `json:"id"`
		}
		if ans == nil || json.Unmarshal(ans, &r) != nil || r.ID <= 0 {
			a.Kind, a.Reason = Drop, "HTTP 201: the answer has no deployment id, or is too large"
			break
		}
		a.Kind, a.ID = Sent, r.ID
	case code >= 300 && code < 400:
		a.Kind, a.Reason = Drop, fmt.Sprintf("HTTP %d: the repo moved. Fix: set repository in [github] to its new name", code)
	case code >= 500 && code <= 599:
		a.Kind, a.Reason = Retry, fmt.Sprintf("HTTP %d: an error of GitHub", code)
	case code == http.StatusTooManyRequests:
		a.Kind, a.Reason = Retry, "HTTP 429: the rate limit of GitHub"
	case code == http.StatusForbidden && (hdr.Get("X-Ratelimit-Remaining") == "0" || hdr.Get("Retry-After") != "" || bytes.Contains(bytes.ToLower(ans), []byte("rate limit"))):
		a.Kind, a.Reason = Retry, "HTTP 403: the rate limit of GitHub"
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		a.Kind, a.Reason, a.Refused = Retry, fmt.Sprintf("HTTP %d: the token is refused", code), true
	case code == http.StatusNotFound && p.Create:
		a.Kind, a.Reason, a.Refused = Retry, "HTTP 404: the repo is not found with this token", true
	case code == http.StatusNotFound:
		a.Kind, a.Reason = Drop, "HTTP 404: the deployment is not on GitHub any more"
	case code == http.StatusConflict || code == http.StatusUnprocessableEntity:
		a.Kind, a.Reason = Drop, fmt.Sprintf("HTTP %d: GitHub did not take the post (for example, the commit is not on GitHub)", code)
	default:
		a.Kind, a.Reason = Drop, fmt.Sprintf("HTTP %d: an answer that pullgate does not take", code)
	}
	return a
}

// Get calls GET path of the repo (doctor) and returns the HTTP code.
// It writes nothing.
func (c *Client) Get(ctx context.Context, path string) (int, error) {
	code, _, _, err := c.do(ctx, http.MethodGet, c.repoURL()+path, nil)
	if err != nil && code == 0 {
		return 0, errors.New("no answer from GitHub (a network error, a TLS error or a timeout)")
	}
	return code, nil
}

// errBody is a network error while the answer was read.
var errBody = errors.New("the answer was cut off")

// do makes one call. The answer is read up to maxAnswer bytes; a larger
// one comes back as nil. A read error comes back with the code and
// errBody. The error never holds the token or the url.
func (c *Client) do(ctx context.Context, method, url string, body []byte) (int, http.Header, []byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return 0, nil, nil, errors.New("bad request")
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "pullgate")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	h := c.HTTP
	if h == nil {
		h = NewHTTPClient(nil)
	}
	resp, err := h.Do(req)
	if err != nil {
		return 0, nil, nil, errors.New("no answer")
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, errBody
	}
	if len(b) > maxAnswer {
		b = nil
	}
	return resp.StatusCode, resp.Header, b, nil
}

// Sender sends the queue of one instance.
type Sender struct {
	Q        *Queue
	C        *Client
	Log      *slog.Logger
	Now      func() time.Time // the clock of the 24-hour limit
	Deadline time.Time        // the real time when this process stops sending
	Instance string
	// Refused is called with the fixed text of the token-refused notice,
	// at most once a day.
	Refused func(text string)
}

// RefusedText is the fixed text of the token-refused log line and notice.
func RefusedText(instance, repo string) string {
	return "the GitHub token of instance " + instance + " is refused: make a new one for " + repo + " with Deployments: read and write"
}

// Send sends the posts in order until the queue is empty, a post must be
// tried again, or the deadline. It never returns an error: problems are
// logged, and kept in Last for pullgate status.
func (s *Sender) Send(ctx context.Context) {
	q := s.Q
	changed := false
	defer func() {
		if changed {
			q.prune()
			if err := q.Save(); err != nil {
				s.Log.Error("save the GitHub queue", "err", err)
			}
		}
	}()
	for len(q.Posts) > 0 {
		left := time.Until(s.Deadline)
		if left <= 0 {
			s.Log.Info("github: the send time is up; the rest waits for the next run", "waiting", len(q.Posts))
			return
		}
		p := q.Posts[0]
		d := q.find(p.Key)
		now := s.Now()
		switch {
		case d == nil || d.Dropped:
			q.Posts = q.Posts[1:]
			changed = true
			continue
		case now.Sub(p.Queued) >= MaxAge:
			s.Log.Warn("github: a post is older than 24 hours; dropped, with the statuses that depend on it", "post", p.What(), "sha", d.SHA)
			q.Last = &Last{At: now, What: p.What(), Error: "not sent in 24 hours; dropped"}
			q.drop(d)
			changed = true
			continue
		case d.Repository != s.C.Repository || d.API != s.C.API || d.Environment != s.C.Environment:
			// The config changed: never send it to another repo or server,
			// where its id could name another deployment.
			s.Log.Warn("github: [github] changed since the post was queued; dropped", "post", p.What(), "sha", d.SHA)
			q.drop(d)
			changed = true
			continue
		case !p.Create && d.ID == 0:
			// Its create was never sent: it cannot be sent.
			s.Log.Warn("github: a status without its deployment; dropped", "post", p.What(), "sha", d.SHA)
			q.drop(d)
			changed = true
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, min(left, CallTimeout))
		a := s.C.Post(cctx, d, p)
		cancel()
		changed = true
		q.Last = &Last{At: now, What: p.What(), OK: a.Kind == Sent, Error: a.Reason}
		switch a.Kind {
		case Sent:
			if p.Create {
				d.ID = a.ID
			}
			q.Posts = q.Posts[1:]
			s.Log.Info("github: posted", "post", p.What(), "sha", d.SHA)
			// Saved after each post, so a crash does not send it twice.
			if err := q.Save(); err != nil {
				s.Log.Error("save the GitHub queue", "err", err)
				return
			}
		case Retry:
			if a.Refused {
				s.refused(now)
			}
			s.Log.Warn("github: the post waits for the next run", "post", p.What(), "sha", d.SHA, "reason", a.Reason)
			return
		default:
			s.Log.Warn("github: the post is dropped, with the statuses that depend on it", "post", p.What(), "sha", d.SHA, "reason", a.Reason)
			q.drop(d)
		}
	}
}

func (s *Sender) refused(now time.Time) {
	q := s.Q
	if !q.RefusedNote.IsZero() && now.Sub(q.RefusedNote) < 24*time.Hour && !q.RefusedNote.After(now) {
		return
	}
	prev := q.RefusedNote
	q.RefusedNote = now
	// Saved before the notice, so a crash never repeats it the same day.
	if err := q.Save(); err != nil {
		q.RefusedNote = prev // not sent: the next try sends it
		s.Log.Error("save the GitHub queue; the token-refused notice waits", "err", err)
		return
	}
	text := RefusedText(s.Instance, s.C.Repository)
	s.Log.Error(text)
	if s.Refused != nil {
		s.Refused(text)
	}
}
