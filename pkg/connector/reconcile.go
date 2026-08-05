package connector

import (
	"context"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/id"
)

// reconcileJoinedRooms bridges accepted chats that /sync refuses to show.
//
// Reddit caps the sync room list at 20 joined and 20 invited rooms, and no filter, timeline limit
// or full_state lifts it - verified against the live server, where /joined_rooms returned 23 rooms
// while every sync variant returned 20. Without this, an account with more than 20 chats would
// silently lose the remainder, with no error anywhere to explain it.
//
// This runs once per connection, after the first sync. Rooms already covered by that sync are
// skipped, so the usual cost is one request plus a handful of resyncs.
func (c *RedditChatClient) reconcileJoinedRooms(ctx context.Context, syncedRooms map[id.RoomID]bool) {
	log := zerolog.Ctx(ctx)
	rooms, err := c.Client.JoinedRooms(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to list joined Reddit rooms, sync-capped rooms may be missing")
		return
	}
	missing := 0
	for _, roomID := range rooms {
		if ctx.Err() != nil {
			return
		}
		if syncedRooms[roomID] {
			continue
		}
		// On a restart the first sync is incremental and reports almost nothing, so the sync set
		// alone would mark every room as missing. An existing portal means it is already bridged.
		if portal, err := c.Main.br.GetExistingPortalByKey(ctx, c.portalKey(roomID)); err != nil {
			log.Debug().Err(err).Stringer("room_id", roomID).Msg("Failed to look up portal")
		} else if portal != nil {
			continue
		}
		if !c.Main.Config.BridgeHiddenChats {
			hidden, err := c.Client.IsHiddenChat(ctx, roomID)
			if err != nil {
				log.Debug().Err(err).Stringer("room_id", roomID).Msg("Failed to check hidden status")
			} else if hidden {
				continue
			}
		}
		// Counted only after the skip checks, so the number reflects rooms actually bridged.
		missing++
		log.Debug().Stringer("room_id", roomID).Msg("Bridging chat that /sync did not report")
		c.Main.br.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    c.portalKey(roomID),
				CreatePortal: true,
			},
			GetChatInfoFunc: c.getChatInfo,
		})
	}
	// Resync portals that already exist as well. Reddit's Matrix layer has no avatars, so ghost
	// info can only come from reddit.com, and an already-named ghost is never refreshed
	// otherwise - existing portals would keep blank avatars forever. The per-user info cache
	// keeps this to one reddit.com request per participant.
	for _, roomID := range rooms {
		if ctx.Err() != nil {
			return
		}
		portal, err := c.Main.br.GetExistingPortalByKey(ctx, c.portalKey(roomID))
		if err != nil || portal == nil {
			continue
		}
		c.Main.br.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatResync,
				PortalKey: portal.PortalKey,
			},
			GetChatInfoFunc: c.getChatInfo,
		})
	}

	if missing > 0 {
		log.Info().
			Int("total_joined", len(rooms)).
			Int("newly_bridged", missing).
			Msg("Bridged chats that Reddit's capped sync room list omitted")
	}
}

// syncedRoomSet collects the room IDs a sync response covered, so reconciliation only looks at
// what sync left out.
func syncedRoomSet(resp *mautrix.RespSync) map[id.RoomID]bool {
	set := make(map[id.RoomID]bool, len(resp.Rooms.Join)+len(resp.Rooms.Invite))
	for roomID := range resp.Rooms.Join {
		set[roomID] = true
	}
	for roomID := range resp.Rooms.Invite {
		set[roomID] = true
	}
	return set
}
