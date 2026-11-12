// Package faketg is a fake Telegram Bot API server for tests.
package faketg

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/code6x/pullgate/internal/telegram"
)

// Sent is one message that the bot sent or edited.
type Sent struct {
	Method  string
	Chat    int64
	ID      int64
	Text    string
	Buttons []telegram.Button
	ReplyTo int64
}

// Server is the fake server.
type Server struct {
	*httptest.Server
	Token string

	mu      sync.Mutex
	nextID  int64
	updates []telegram.Update
	nextUp  int64
	msgs    map[int64]*telegram.Message
	Sent    []Sent
	Answers []string
	wake    chan struct{}
}

// New starts a fake server for token.
func New(t testing.TB, token string) *Server {
	s := &Server{Token: token, nextID: 100, nextUp: 1, msgs: map[int64]*telegram.Message{}, wake: make(chan struct{}, 1)}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	prefix := "/bot" + s.Token + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeJSON(w, map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
		return
	}
	method := strings.TrimPrefix(r.URL.Path, prefix)
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	num := func(k string) int64 { f, _ := req[k].(float64); return int64(f) }
	str := func(k string) string { v, _ := req[k].(string); return v }
	switch method {
	case "deleteWebhook":
		writeJSON(w, map[string]any{"ok": true, "result": true})
	case "getUpdates":
		// Like Telegram, give only the kinds in allowed_updates.
		allowed := map[string]bool{}
		if l, ok := req["allowed_updates"].([]any); ok {
			for _, k := range l {
				if ks, ok := k.(string); ok {
					allowed[ks] = true
				}
			}
		}
		want := func(u telegram.Update) bool {
			if u.UpdateID < num("offset") {
				return false
			}
			switch {
			case len(allowed) == 0:
				return true
			case u.CallbackQuery != nil:
				return allowed["callback_query"]
			case u.Message != nil:
				return allowed["message"]
			case u.EditedMessage != nil:
				return allowed["edited_message"]
			}
			return false
		}
		s.mu.Lock()
		var out []telegram.Update
		for _, u := range s.updates {
			if want(u) {
				out = append(out, u)
			}
		}
		s.mu.Unlock()
		if len(out) == 0 && num("timeout") > 0 {
			select {
			case <-s.wake:
			case <-r.Context().Done():
			}
			s.mu.Lock()
			for _, u := range s.updates {
				if want(u) {
					out = append(out, u)
				}
			}
			s.mu.Unlock()
		}
		if out == nil {
			out = []telegram.Update{}
		}
		writeJSON(w, map[string]any{"ok": true, "result": out})
	case "sendMessage", "editMessageText":
		s.mu.Lock()
		sent := Sent{Method: method, Chat: num("chat_id"), Text: str("text")}
		if rm, ok := req["reply_markup"].(map[string]any); ok {
			if kb, ok := rm["inline_keyboard"].([]any); ok && len(kb) > 0 {
				for _, b := range kb[0].([]any) {
					bm := b.(map[string]any)
					sent.Buttons = append(sent.Buttons, telegram.Button{Text: bm["text"].(string), CallbackData: bm["callback_data"].(string)})
				}
			}
		}
		if rp, ok := req["reply_parameters"].(map[string]any); ok {
			f, _ := rp["message_id"].(float64)
			sent.ReplyTo = int64(f)
		}
		var m *telegram.Message
		if method == "sendMessage" {
			s.nextID++
			m = &telegram.Message{MessageID: s.nextID, Chat: telegram.Chat{ID: sent.Chat}, Text: sent.Text}
			s.msgs[m.MessageID] = m
		} else {
			m = s.msgs[num("message_id")]
			if m == nil {
				s.mu.Unlock()
				writeJSON(w, map[string]any{"ok": false, "error_code": 400, "description": "message not found"})
				return
			}
			m.Text = sent.Text
			m.EditDate = 1
		}
		sent.ID = m.MessageID
		s.Sent = append(s.Sent, sent)
		cp := *m
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "result": cp})
	case "answerCallbackQuery":
		s.mu.Lock()
		s.Answers = append(s.Answers, str("text"))
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "result": true})
	default:
		writeJSON(w, map[string]any{"ok": false, "error_code": 404, "description": "Not Found"})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Tap queues a button tap by user from on message msg in chat, with data.
func (s *Server) Tap(from, chat, msg int64, data string) {
	s.mu.Lock()
	m := s.msgs[msg]
	var cp *telegram.Message
	if m != nil {
		c := *m
		c.Chat = telegram.Chat{ID: chat}
		cp = &c
	} else {
		cp = &telegram.Message{MessageID: msg, Chat: telegram.Chat{ID: chat}}
	}
	s.updates = append(s.updates, telegram.Update{UpdateID: s.nextUp, CallbackQuery: &telegram.CallbackQuery{
		ID: "cq", From: telegram.User{ID: from}, Message: cp, Data: data,
	}})
	s.nextUp++
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Say queues a message with text from user from in chat, as getUpdates
// gives it. With edited set, it comes as an edited message.
func (s *Server) Say(from, chat int64, text string, edited bool) {
	s.mu.Lock()
	s.nextID++
	m := &telegram.Message{MessageID: s.nextID, From: &telegram.User{ID: from}, Chat: telegram.Chat{ID: chat}, Text: text}
	u := telegram.Update{UpdateID: s.nextUp, Message: m}
	if edited {
		m.EditDate = 3
		u = telegram.Update{UpdateID: s.nextUp, EditedMessage: m}
	}
	s.updates = append(s.updates, u)
	s.nextUp++
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// EditByThief changes the text of a message as a token thief would.
func (s *Server) EditByThief(msg int64, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.msgs[msg]; m != nil {
		m.Text = text
		m.EditDate = 2
	}
}

// Messages returns a copy of what was sent.
func (s *Server) Messages() []Sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sent(nil), s.Sent...)
}

// Pending reports how many taps getUpdates has not confirmed yet is not
// tracked; Updates returns the number of queued updates.
func (s *Server) Updates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.updates)
}
