package redditchat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func hiddenTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(Config{HomeserverURL: srv.URL, ServerName: "reddit.com", RequestTimeout: 5 * time.Second}, "@t2_3mbr7:reddit.com", "tok")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Pressing Ignore on a Reddit chat request leaves no trace anywhere except this account data
// key - verified by diffing a room byte-for-byte before and after the action.
func TestIsHiddenChat(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"dismissed", 200, `{"hidden":true}`, true},
		{"explicitly not hidden", 200, `{"hidden":false}`, false},
		{"never dismissed (404)", 404, `{"errcode":"M_NOT_FOUND","error":"data not found"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := hiddenTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "account_data/"+HiddenChatAccountData) {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			got, err := c.IsHiddenChat(context.Background(), "!r:reddit.com")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("IsHiddenChat = %v, want %v", got, tc.want)
			}
		})
	}
}

// Reddit writes the spam status more than once - "unverified" when the room is created, then the
// real verdict a second or two later. Pagination returns newest-first, so the first match wins.
func TestIsSpamChatUsesNewestStatus(t *testing.T) {
	newest := func(status ...string) string {
		chunk := make([]map[string]any, 0, len(status))
		for _, s := range status {
			chunk = append(chunk, map[string]any{
				"type": InviteSpamStatusEvent, "content": map[string]any{"status": s},
				"event_id": "$x", "sender": "@t2_a:reddit.com", "origin_server_ts": 1,
			})
		}
		b, _ := json.Marshal(map[string]any{"chunk": chunk, "start": "s", "end": "e"})
		return string(b)
	}
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"classified spam", newest("spam", "unverified"), true},
		{"classified clean", newest("verified", "unverified"), false},
		{"still unverified", newest("unverified"), false},
		{"no status at all", `{"chunk":[],"start":"s","end":"e"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := hiddenTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			got, err := c.IsSpamChat(context.Background(), "!r:reddit.com")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("IsSpamChat = %v, want %v", got, tc.want)
			}
		})
	}
}

// For accepted chats the flag arrives in sync account data, so no extra request is needed.
func TestIsHiddenChatInSync(t *testing.T) {
	mk := func(evtType string, content map[string]any) *event.Event {
		return &event.Event{Type: event.Type{Type: evtType}, Content: event.Content{Raw: content}}
	}
	if !IsHiddenChatInSync([]*event.Event{mk(HiddenChatAccountData, map[string]any{"hidden": true})}) {
		t.Error("hidden:true should be detected")
	}
	if IsHiddenChatInSync([]*event.Event{mk(HiddenChatAccountData, map[string]any{"hidden": false})}) {
		t.Error("hidden:false is not hidden")
	}
	if IsHiddenChatInSync([]*event.Event{mk("m.fully_read", map[string]any{"event_id": "$x"})}) {
		t.Error("unrelated account data is not hidden")
	}
	if IsHiddenChatInSync(nil) {
		t.Error("no account data is not hidden")
	}
	_ = id.RoomID("")
}
