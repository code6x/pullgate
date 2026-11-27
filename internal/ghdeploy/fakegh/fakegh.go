// Package fakegh is a fake GitHub deployments API, for tests. It never
// calls the real GitHub.
package fakegh

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Call is one request the server got.
type Call struct {
	Method, Path, Auth string
	Body               map[string]any
	// SHA is the ref of the deployment: of the create itself, or of the
	// deployment whose status this is.
	SHA string
}

// Step is the call in short: "create SHA" or "STATE SHA DESCRIPTION".
func (c Call) Step() string {
	if c.Method != http.MethodPost {
		return c.Method + " " + c.Path
	}
	if strings.HasSuffix(c.Path, "/statuses") {
		return fmt.Sprintf("%v %s %v", c.Body["state"], c.SHA, c.Body["description"])
	}
	return "create " + c.SHA
}

// Server is the fake API.
type Server struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []Call           // the posts that were taken (201)
	all    []Call           // every request, also the refused ones
	ids    map[int64]string // deployment id -> ref
	nextID int64
	// Answer gives the HTTP code of a call (0: as GitHub would) and an
	// optional body. It runs before the call is taken.
	Answer func(c Call) (int, string)
	hang   chan struct{}
}

// New starts a server on 127.0.0.1 (plain HTTP); it stops at the end of
// the test.
func New(t testing.TB) *Server {
	s := &Server{ids: map[int64]string{}, nextID: 1000}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Hang makes every call wait until the end of the test: a GitHub that
// never answers.
func (s *Server) Hang(t testing.TB) {
	s.mu.Lock()
	s.hang = make(chan struct{})
	h := s.hang
	s.mu.Unlock()
	t.Cleanup(func() { close(h) })
}

// Calls are the calls so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// All are all requests so far, also the refused ones and the GETs.
func (s *Server) All() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.all...)
}

// Steps are the posts so far, in short, that GitHub took (201).
func (s *Server) Steps() []string {
	var out []string
	for _, c := range s.Calls() {
		out = append(out, c.Step())
	}
	return out
}

// Reset forgets the calls.
func (s *Server) Reset() {
	s.mu.Lock()
	s.calls, s.all = nil, nil
	s.mu.Unlock()
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h := s.hang
	s.mu.Unlock()
	if h != nil {
		select {
		case <-h:
		case <-r.Context().Done():
		}
		return
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	c := Call{Method: r.Method, Path: r.URL.RequestURI(), Auth: r.Header.Get("Authorization")}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &c.Body)
	}
	s.mu.Lock()
	if c.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/deployments") {
		c.SHA, _ = c.Body["ref"].(string)
	} else if parts := strings.Split(r.URL.Path, "/"); len(parts) > 2 && parts[len(parts)-1] == "statuses" {
		id, _ := strconv.ParseInt(parts[len(parts)-2], 10, 64)
		c.SHA = s.ids[id]
	}
	answer := s.Answer
	s.all = append(s.all, c)
	s.mu.Unlock()
	code, body := 0, ""
	if answer != nil {
		code, body = answer(c)
	}
	if code != 0 && code != http.StatusCreated {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
		return
	}
	if code == 0 && c.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `[]`)
		return
	}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.nextID++
	id := s.nextID
	if c.SHA != "" && !strings.HasSuffix(r.URL.Path, "/statuses") {
		s.ids[id] = c.SHA
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	if body == "" {
		body = fmt.Sprintf(`{"id": %d, "node_id": "x"}`, id)
	}
	_, _ = io.WriteString(w, body)
}
