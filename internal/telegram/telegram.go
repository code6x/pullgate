// Package telegram is a small Bot API client. Only the listener uses it
// (rule 4). The token is part of every URL, so no error from here ever
// holds a URL or the token.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client calls the Bot API.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for apiURL (https://api.telegram.org).
func New(apiURL, token string) *Client {
	return &Client{base: strings.TrimRight(apiURL, "/"), token: token, http: &http.Client{Timeout: 90 * time.Second}}
}

// String hides the token.
func (c *Client) String() string { return "telegram.Client{token: hidden}" }

// User is a Telegram user.
type User struct {
	ID int64 `json:"id"`
}

// Chat is a Telegram chat.
type Chat struct {
	ID int64 `json:"id"`
}

// Message is a Telegram message.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
	EditDate  int64  `json:"edit_date,omitempty"`
}

// CallbackQuery is a button tap.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data"`
}

// Update is one update of getUpdates.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	Message       *Message       `json:"message,omitempty"`
	EditedMessage *Message       `json:"edited_message,omitempty"`
}

// Button is an inline keyboard button.
type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// APIError is an error answer of the Bot API.
type APIError struct {
	Method string
	Code   int
	Desc   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: error %d: %s", e.Method, e.Code, e.Desc)
}

// Permanent reports whether retrying cannot help (a 4xx other than 429).
func (e *APIError) Permanent() bool { return e.Code >= 400 && e.Code < 500 && e.Code != 429 }

type answer struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
}

func (c *Client) call(ctx context.Context, method string, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: bad api_url", method)
	}
	hr.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(hr)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the url.Error holds the URL, and the URL holds the token
		}
		return fmt.Errorf("telegram %s: %s", method, c.hide(err.Error()))
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: read: %s", method, c.hide(err.Error()))
	}
	var a answer
	if err := json.Unmarshal(b, &a); err != nil {
		return &APIError{Method: method, Code: res.StatusCode, Desc: "the answer is not JSON"}
	}
	if !a.OK {
		code := a.ErrorCode
		if code == 0 {
			code = res.StatusCode
		}
		return &APIError{Method: method, Code: code, Desc: c.hide(Clip(a.Description))}
	}
	if resp != nil {
		if err := json.Unmarshal(a.Result, resp); err != nil {
			return fmt.Errorf("telegram %s: bad result: %w", method, err)
		}
	}
	return nil
}

func (c *Client) hide(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "<token>")
}

// Clip cuts a text from Telegram for a log line.
func Clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// DeleteWebhook removes a webhook, so getUpdates works.
func (c *Client) DeleteWebhook(ctx context.Context) error {
	return c.call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

// GetUpdates long-polls for button taps and messages (for /check).
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeout, "allowed_updates": []string{"callback_query", "message"},
	}, &ups)
	return ups, err
}

// SendMessage sends an HTML message, with buttons and as a reply when set.
func (c *Client) SendMessage(ctx context.Context, chat int64, text string, buttons []Button, replyTo int64) (*Message, error) {
	req := map[string]any{
		"chat_id": chat, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if len(buttons) > 0 {
		req["reply_markup"] = map[string]any{"inline_keyboard": [][]Button{buttons}}
	}
	if replyTo != 0 {
		req["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	var m Message
	if err := c.call(ctx, "sendMessage", req, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// EditMessageText replaces the text of a message and removes its buttons.
func (c *Client) EditMessageText(ctx context.Context, chat, msg int64, text string) error {
	return c.call(ctx, "editMessageText", map[string]any{
		"chat_id": chat, "message_id": msg, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
		"reply_markup":         map[string]any{"inline_keyboard": [][]Button{}},
	}, nil)
}

// AnswerCallbackQuery answers a tap.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}
