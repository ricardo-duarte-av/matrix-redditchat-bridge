package connector

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

// realInviteState is the invite_state of an unaccepted Reddit chat, captured from a live account
// with the other participant's ID and username replaced. Reddit answers 403 for /members and
// /state on these rooms, so this is the only source of chat info the bridge has for them.
const realInviteState = `[
  {"type":"m.room.create","state_key":"","sender":"@t2_examplesender:reddit.com","content":{"creator":"@t2_examplesender:reddit.com","room_version":"7"}},
  {"type":"m.room.join_rules","state_key":"","sender":"@t2_examplesender:reddit.com","content":{"join_rule":"invite"}},
  {"type":"m.room.member","state_key":"@t2_examplesender:reddit.com","sender":"@t2_examplesender:reddit.com","content":{"displayname":"ExampleSender","membership":"join"}},
  {"type":"com.reddit.chat.type","state_key":"","sender":"@t2_examplesender:reddit.com","content":{"participants":["@t2_examplesender:reddit.com","@t2_3mbr7:reddit.com"],"type":"direct"}},
  {"type":"m.room.member","state_key":"@t2_3mbr7:reddit.com","sender":"@t2_examplesender:reddit.com","content":{"displayname":"daedric","is_direct":true,"membership":"invite"}}
]`

func testClient(t *testing.T) *RedditChatClient {
	t.Helper()
	client, err := redditchat.NewClient(redditchat.Config{
		HomeserverURL:  "https://matrix.redditspace.com",
		ServerName:     "reddit.com",
		RequestTimeout: time.Second,
	}, "@t2_3mbr7:reddit.com", "token")
	if err != nil {
		t.Fatal(err)
	}
	return &RedditChatClient{
		Client: client,
		UserLogin: &bridgev2.UserLogin{
			UserLogin: &database.UserLogin{
				ID:       networkid.UserLoginID("t2_3mbr7"),
				Metadata: &UserLoginMetadata{},
			},
		},
	}
}

func parseState(t *testing.T, raw string) []*event.Event {
	t.Helper()
	var evts []*event.Event
	if err := json.Unmarshal([]byte(raw), &evts); err != nil {
		t.Fatal(err)
	}
	return evts
}

// An unaccepted Reddit chat must become a portal marked as a message request, with both
// participants present, built purely from invite_state.
func TestChatInfoFromInviteState(t *testing.T) {
	c := testClient(t)
	info := c.chatInfoFromInviteState(context.Background(), parseState(t, realInviteState))

	if info.MessageRequest == nil || !*info.MessageRequest {
		t.Error("an unaccepted Reddit chat must be marked as a message request")
	}
	if info.Type == nil || *info.Type != database.RoomTypeDM {
		t.Errorf("a two-participant direct chat should be a DM, got %v", info.Type)
	}
	if info.Members.TotalMemberCount != 2 {
		t.Fatalf("member count = %d, want 2", info.Members.TotalMemberCount)
	}
	if !info.CanBackfill {
		t.Error("history is readable without accepting, so backfill should be allowed")
	}

	me, ok := info.Members.MemberMap[networkid.UserID("t2_3mbr7")]
	if !ok {
		t.Fatal("own user missing from member list")
	}
	if !me.IsFromMe {
		t.Error("own member entry should be marked IsFromMe so double puppeting works")
	}
	if me.Membership != event.MembershipInvite {
		t.Errorf("own membership = %q, want invite", me.Membership)
	}

	them, ok := info.Members.MemberMap[networkid.UserID("t2_examplesender")]
	if !ok {
		t.Fatal("other participant missing from member list")
	}
	if them.IsFromMe {
		t.Error("the other participant must not be marked IsFromMe")
	}
	if them.Membership != event.MembershipJoin {
		t.Errorf("other membership = %q, want join", them.Membership)
	}
}

// Reddit reports "has never joined this room" as a 403, not a 404, and that must be read as
// "still a pending request" rather than as a hard failure.
func TestIsNotJoined(t *testing.T) {
	forbidden := mautrix.HTTPError{RespError: &mautrix.RespError{ErrCode: "M_FORBIDDEN", Err: "user has never joined this room"}}
	if !isNotJoined(forbidden) {
		t.Error("M_FORBIDDEN should read as not-joined")
	}
	notFound := mautrix.HTTPError{RespError: &mautrix.RespError{ErrCode: "M_NOT_FOUND"}}
	if isNotJoined(notFound) {
		t.Error("M_NOT_FOUND is a missing state event, not a permission problem")
	}
	if isNotJoined(nil) {
		t.Error("nil is not an error")
	}
}
