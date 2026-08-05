package connector

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

// TestLiveInviteState runs the invite_state conversion against a real Reddit account.
// Skipped unless RC_COOKIE_FILE is set.
func TestLiveInviteState(t *testing.T) {
	path := os.Getenv("RC_COOKIE_FILE")
	if path == "" {
		t.Skip("set RC_COOKIE_FILE to run against a live account")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := (&redditchat.TokenRefresher{Cookie: strings.TrimSpace(string(raw))}).Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	client, err := redditchat.NewClient(redditchat.Config{
		HomeserverURL: "https://matrix.redditspace.com", ServerName: "reddit.com", RequestTimeout: 60 * time.Second,
	}, "", ref.Token)
	if err != nil {
		t.Fatal(err)
	}
	me, err := client.Whoami(ctx)
	if err != nil {
		t.Fatal(err)
	}
	localpart, _, _ := me.Parse()
	sync, err := client.Sync(ctx, "", 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := &RedditChatClient{Client: client, UserLogin: &bridgev2.UserLogin{
		UserLogin: &database.UserLogin{
			ID:       networkid.UserLoginID(localpart),
			Metadata: &UserLoginMetadata{},
		},
	}}

	t.Logf("%d joined, %d pending chat requests", len(sync.Rooms.Join), len(sync.Rooms.Invite))
	for roomID, inv := range sync.Rooms.Invite {
		info := c.chatInfoFromInviteState(ctx, inv.State.Events)
		if info.MessageRequest == nil || !*info.MessageRequest {
			t.Errorf("%s: not marked as a message request", roomID)
		}
		if info.Members.TotalMemberCount < 2 {
			t.Errorf("%s: only %d members resolved from invite_state", roomID, info.Members.TotalMemberCount)
		}
		if info.Type == nil || *info.Type != database.RoomTypeDM {
			t.Errorf("%s: not typed as DM", roomID)
		}
		if _, ok := info.Members.MemberMap[networkid.UserID(localpart)]; !ok {
			t.Errorf("%s: own user missing", roomID)
		}
	}
}
