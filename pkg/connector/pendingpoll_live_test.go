package connector

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

// TestLivePendingRequestInvisibleInSync documents and re-checks the Reddit behaviour the
// pending-request poller exists for: a room the user is only invited to never reports timeline
// activity through /sync, even though /messages returns it immediately.
//
// Skipped unless RC_COOKIE_FILE and RC_PENDING_ROOM are set.
func TestLivePendingRequestInvisibleInSync(t *testing.T) {
	cookiePath := os.Getenv("RC_COOKIE_FILE")
	roomID := os.Getenv("RC_PENDING_ROOM")
	if cookiePath == "" || roomID == "" {
		t.Skip("set RC_COOKIE_FILE and RC_PENDING_ROOM to run against a live account")
	}
	raw, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := (&redditchat.TokenRefresher{Cookie: strings.TrimSpace(string(raw))}).Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := redditchat.NewClient(redditchat.Config{
		HomeserverURL: "https://matrix.redditspace.com", ServerName: "reddit.com", RequestTimeout: 60 * time.Second,
	}, "", ref.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Whoami(ctx); err != nil {
		t.Fatal(err)
	}
	room := id.RoomID(roomID)

	// The room must still be an unaccepted request for this test to mean anything.
	sync, err := c.Sync(ctx, "", 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sync.Rooms.Invite[room]; !ok {
		t.Skipf("%s is not an unaccepted chat request", room)
	}

	// /messages must show the history that sync withholds.
	resp, err := c.Messages(ctx, room, "", mautrix.DirectionBackward, 40)
	if err != nil {
		t.Fatalf("pending requests must be readable via /messages: %v", err)
	}
	msgs := 0
	for _, evt := range resp.Chunk {
		if evt.Type == event.EventMessage {
			msgs++
		}
	}
	if msgs == 0 {
		t.Fatal("expected the pending request to contain messages")
	}
	t.Logf("pending request %s has %d messages readable via /messages while sync withholds them", room, msgs)
}
