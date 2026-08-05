// Command redditchat-inspect dumps what Reddit's /sync actually returns, for figuring out how
// Reddit's concepts map onto Matrix ones. It is a diagnostic tool, not part of the bridge.
//
// Usage:
//
//	RC_COOKIE_FILE=~/.redditcookie go run ./cmd/redditchat-inspect
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

func main() {
	path := os.Getenv("RC_COOKIE_FILE")
	if path == "" {
		fmt.Fprintln(os.Stderr, "set RC_COOKIE_FILE to a file containing your Reddit Cookie header")
		os.Exit(2)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to read cookie file:", err)
		os.Exit(2)
	}

	ctx := context.Background()
	refreshed, err := (&redditchat.TokenRefresher{Cookie: strings.TrimSpace(string(raw))}).Refresh(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "refresh failed:", err)
		os.Exit(1)
	}
	c, err := redditchat.NewClient(redditchat.Config{
		HomeserverURL:  "https://matrix.redditspace.com",
		ServerName:     "reddit.com",
		RequestTimeout: 60 * time.Second,
	}, "", refreshed.Token)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	me, err := c.Whoami(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "whoami failed:", err)
		os.Exit(1)
	}
	sync, err := c.Sync(ctx, "", 15*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync failed:", err)
		os.Exit(1)
	}

	fmt.Printf("logged in as %s\n%d joined rooms, %d invite-membership rooms\n\n", me, len(sync.Rooms.Join), len(sync.Rooms.Invite))
	fmt.Println("=== rooms where Matrix membership is 'invite' ===")
	fmt.Println("(check each of these in Reddit's UI - do they show as a chat, a message request, or nothing at all?)")

	// Sort newest-activity-first so a chat started during a live experiment shows up at the top.
	type entry struct {
		id   id.RoomID
		last time.Time
	}
	entries := make([]entry, 0, len(sync.Rooms.Invite))
	for rid := range sync.Rooms.Invite {
		e := entry{id: rid}
		if msgs, err := c.Messages(ctx, rid, "", mautrix.DirectionBackward, 10); err == nil {
			for _, evt := range msgs.Chunk {
				if ts := time.UnixMilli(evt.Timestamp); ts.After(e.last) {
					e.last = ts
				}
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].last.After(entries[j].last) })
	roomIDs := make([]id.RoomID, len(entries))
	lastActivity := make(map[id.RoomID]time.Time, len(entries))
	for i, e := range entries {
		roomIDs[i] = e.id
		lastActivity[e.id] = e.last
	}

	for i, rid := range roomIDs {
		inv := sync.Rooms.Invite[rid]
		var inviter id.UserID
		others := map[id.UserID]string{}
		var chatType json.RawMessage

		for _, evt := range inv.State.Events {
			switch evt.Type.Type {
			case "m.room.member":
				if evt.StateKey == nil {
					continue
				}
				target := id.UserID(*evt.StateKey)
				_ = redditchat.ParseContent(evt, event.StateMember)
				member := evt.Content.AsMember()
				if target == me && member.Membership == event.MembershipInvite {
					inviter = evt.Sender
				} else if target != me {
					others[target] = member.Displayname
				}
			case "com.reddit.chat.type":
				chatType, _ = json.Marshal(evt.Content.Raw)
			}
		}

		fmt.Printf("\n[%d] %s\n", i+1, rid)
		if ts := lastActivity[rid]; !ts.IsZero() {
			fmt.Printf("     last activity: %s (%s ago)\n", ts.Format(time.RFC3339), time.Since(ts).Truncate(time.Minute))
		}
		if inviter != "" {
			name := others[inviter]
			fmt.Printf("     invited by: %s %s\n", inviter, quoted(name))
			fmt.Printf("     reddit user: https://www.reddit.com/user/%s\n", displayOrID(others[inviter], inviter))
		} else {
			fmt.Println("     invited by: <no inviting member event>")
		}
		for uid, name := range others {
			if uid != inviter {
				fmt.Printf("     other member: %s %s\n", uid, quoted(name))
			}
		}
		if len(chatType) > 0 {
			fmt.Printf("     com.reddit.chat.type: %s\n", chatType)
		}

		// Can we read the room's history without joining? If so, no join is needed to bridge it.
		msgs, err := c.Messages(ctx, rid, "", mautrix.DirectionBackward, 5)
		if err != nil {
			fmt.Printf("     history without joining: DENIED (%v)\n", err)
		} else {
			texts := 0
			for _, e := range msgs.Chunk {
				if e.Type == event.EventMessage {
					texts++
				}
			}
			fmt.Printf("     history without joining: OK (%d events, %d messages)\n", len(msgs.Chunk), texts)
		}
	}
}

func quoted(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("(%q)", s)
}

func displayOrID(name string, uid id.UserID) string {
	if name != "" {
		return name
	}
	localpart, _, _ := uid.Parse()
	return localpart
}
