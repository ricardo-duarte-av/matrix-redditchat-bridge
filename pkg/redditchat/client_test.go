package redditchat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// captureServer records the requests the client makes so they can be compared against the
// requests Reddit's own web client sends.
func captureServer(t *testing.T, body string) (*Client, <-chan *url.URL) {
	t.Helper()
	reqs := make(chan *url.URL, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs <- r.URL
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(Config{
		HomeserverURL:  srv.URL,
		ServerName:     "reddit.com",
		RequestTimeout: 5 * time.Second,
	}, "@t2_3mbr7:reddit.com", "token")
	if err != nil {
		t.Fatal(err)
	}
	return client, reqs
}

func TestSyncRequestShape(t *testing.T) {
	client, reqs := captureServer(t, `{"next_batch":"s1"}`)
	if _, err := client.Sync(context.Background(), "s0", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	u := <-reqs

	if u.Path != "/_matrix/client/v3/sync" {
		t.Errorf("path = %q, want /_matrix/client/v3/sync", u.Path)
	}
	q := u.Query()
	if q.Get("timeout") != "30000" {
		t.Errorf("timeout = %q, want 30000", q.Get("timeout"))
	}
	if q.Get("since") != "s0" {
		t.Errorf("since = %q, want s0", q.Get("since"))
	}
	// Reddit's client sends the filter inline as JSON rather than as a filter ID.
	want := `{"room":{"state":{"lazy_load_members":true},"timeline":{"not_types":["com.reddit.review_open","com.reddit.review_close"],"lazy_load_members":true,"unread_thread_notifications":true}}}`
	if got := q.Get("filter"); got != want {
		t.Errorf("filter =\n%s\nwant\n%s", got, want)
	}
}

// Backward pagination must start with `from` omitted. Reddit returns nothing for `from=t0_0`,
// which is its end-of-history marker rather than a starting point.
func TestMessagesOmitsFromOnFirstPage(t *testing.T) {
	client, reqs := captureServer(t, `{"start":"t1_1","end":"t0_0","chunk":[]}`)
	roomID := "!yd_DkNOyvHCvfNm4y9Tt-OmWkoidjKIbKNEUkhLZ0Qo:reddit.com"
	if _, err := client.Messages(context.Background(), id.RoomID(roomID), "", mautrix.DirectionBackward, 100); err != nil {
		t.Fatal(err)
	}
	u := <-reqs

	wantPath := "/_matrix/client/v3/rooms/" + roomID + "/messages"
	if u.Path != wantPath {
		t.Errorf("path = %q, want %q", u.Path, wantPath)
	}
	q := u.Query()
	if q.Get("from") != "" {
		t.Errorf("from = %q, want it omitted on the first backward page", q.Get("from"))
	}
	if q.Get("dir") != "b" || q.Get("limit") != "100" {
		t.Errorf("dir/limit = %q/%q, want b/100", q.Get("dir"), q.Get("limit"))
	}
	if q.Get("filter") == "" {
		t.Error("filter is missing")
	}
}

func TestHasMoreHistory(t *testing.T) {
	msg := func(end string, chunk int) *mautrix.RespMessages {
		r := &mautrix.RespMessages{End: end}
		r.Chunk = make([]*event.Event, chunk)
		return r
	}
	cases := []struct {
		name string
		resp *mautrix.RespMessages
		want bool
	}{
		{"normal cursor with events", msg("t46_1686508321661", 5), true},
		{"end-of-history token", msg(EndOfTimelineToken, 5), false},
		{"empty end token", msg("", 5), false},
		{"empty chunk", msg("t46_168", 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasMoreHistory(tc.resp); got != tc.want {
				t.Errorf("HasMoreHistory = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseTokenClaims(t *testing.T) {
	// Payload of a real Reddit chat JWT with the signature and scope claim stripped.
	token := "e30." + "eyJzdWIiOiJ1c2VyIiwiZXhwIjoxNzg1OTYyMjE5LjgyOTA5NiwiaWF0IjoxNzg1ODc1ODE5LjgyOTA5NSwibGlkIjoidDJfM21icjciLCJhaWQiOiJ0Ml8zbWJyNyJ9" + ".sig"
	claims, err := ParseToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.AccountID != "t2_3mbr7" {
		t.Errorf("AccountID = %q, want t2_3mbr7", claims.AccountID)
	}
	// The claims carry fractional seconds, so compare at second resolution.
	if lifetime := claims.ExpiresAt.Sub(claims.IssuedAt).Round(time.Second); lifetime != 24*time.Hour {
		t.Errorf("token lifetime = %v, want 24h", lifetime)
	}
	if !UserIDRegex.MatchString(claims.AccountID) {
		t.Errorf("AccountID %q doesn't match the Reddit user ID regex", claims.AccountID)
	}
}

func TestTokenExpiry(t *testing.T) {
	expired := &TokenClaims{ExpiresAt: time.Now().Add(-time.Minute)}
	if !expired.Expired() {
		t.Error("a token that expired a minute ago should read as expired")
	}
	fresh := &TokenClaims{ExpiresAt: time.Now().Add(23 * time.Hour)}
	if fresh.Expired() {
		t.Error("a token with 23h left should not read as expired")
	}
	if fresh.ExpiresWithin(30 * time.Minute) {
		t.Error("a token with 23h left should not be within the 30m warning window")
	}
	if !fresh.ExpiresWithin(24 * time.Hour) {
		t.Error("a token with 23h left should be within a 24h window")
	}
	// A token with no exp claim must never be treated as expired.
	none := &TokenClaims{}
	if none.Expired() || none.ExpiresWithin(time.Hour) {
		t.Error("claims with no expiry should never report expiry")
	}
}

// mautrix parses event content eagerly on some code paths, and ParseRaw then reports
// "content is already parsed" as an error. Treating that as a failure silently dropped every
// member and every message, so it's pinned here.
func TestParseContentAcceptsAlreadyParsed(t *testing.T) {
	raw := []byte(`{"membership":"join","displayname":"daedric"}`)

	fresh := &event.Event{Type: event.StateMember, Content: event.Content{VeryRaw: raw}}
	if err := ParseContent(fresh, event.StateMember); err != nil {
		t.Fatalf("unparsed content should parse: %v", err)
	}
	if fresh.Content.AsMember().Membership != event.MembershipJoin {
		t.Error("membership not parsed")
	}

	// Second call on the same event is the already-parsed case.
	if err := ParseContent(fresh, event.StateMember); err != nil {
		t.Errorf("already-parsed content should not be an error, got: %v", err)
	}
	if fresh.Content.AsMember().Displayname != "daedric" {
		t.Error("re-parsing clobbered the content")
	}

	// A genuinely unsupported type must still surface as an error.
	bad := &event.Event{Content: event.Content{VeryRaw: raw}}
	if err := ParseContent(bad, event.Type{Type: "com.reddit.nonsense", Class: event.MessageEventType}); err == nil {
		t.Error("unsupported content type should still error")
	}
}
