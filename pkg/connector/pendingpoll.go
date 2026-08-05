package connector

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

// pollPendingRequests works around a Reddit limitation: /sync never reports timeline activity in
// a room the user has only been invited to. Verified against the live server - a message sent to
// an unaccepted chat is readable via /messages immediately, but six minutes of incremental syncs
// never mentioned the room.
//
// Without this, a bridged chat request would show its first message and then go silent until the
// user replied, quietly dropping anything sent in between. Accepted chats are unaffected and are
// never polled, so this costs one request per pending portal per interval and nothing otherwise.
func (c *RedditChatClient) pollPendingRequests(ctx context.Context, done chan struct{}) {
	defer close(done)
	interval := c.Main.Config.PendingRequestPollInterval
	if interval <= 0 {
		return
	}
	log := c.UserLogin.Log.With().Str("component", "pending request poll").Logger()
	ctx = log.WithContext(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pollPendingOnce(ctx)
		}
	}
}

func (c *RedditChatClient) pollPendingOnce(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	portals, err := c.Main.br.GetAllPortals(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to list portals")
		return
	}
	for _, portal := range portals {
		if ctx.Err() != nil {
			return
		}
		// Only this login's still-unaccepted chats. Once accepted, sync covers the room.
		if portal.Receiver != c.UserLogin.ID || !portal.MessageRequest {
			continue
		}
		c.pollPendingPortal(ctx, portal)
	}
}

func (c *RedditChatClient) pollPendingPortal(ctx context.Context, portal *bridgev2.Portal) {
	log := zerolog.Ctx(ctx).With().Str("portal_id", string(portal.ID)).Logger()
	roomID := id.RoomID(portal.ID)

	resp, err := c.Client.Messages(ctx, roomID, "", mautrix.DirectionBackward, c.Main.Config.BackfillBatchSize)
	if err != nil {
		log.Debug().Err(err).Msg("Failed to poll pending chat request")
		return
	}
	// Reddit returns newest-first; queue oldest-first so the portal reads in order.
	for i := len(resp.Chunk) - 1; i >= 0; i-- {
		evt := resp.Chunk[i]
		if evt.Type != event.EventMessage {
			continue
		}
		if err := redditchat.ParseContent(evt, evt.Type); err != nil {
			continue
		}
		content := evt.Content.AsMessage()
		if content.MsgType != event.MsgText && content.MsgType != event.MsgNotice && content.MsgType != event.MsgEmote {
			continue
		}
		// Anything already bridged is dropped by the central module's duplicate check, which
		// keys on this same Reddit event ID.
		c.Main.br.QueueRemoteEvent(c.UserLogin, &simplevent.Message[*event.MessageEventContent]{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventMessage,
				PortalKey: portal.PortalKey,
				Sender:    c.senderOf(evt.Sender),
				Timestamp: time.UnixMilli(evt.Timestamp),
			},
			ID:                 networkid.MessageID(evt.ID),
			Data:               content,
			ConvertMessageFunc: convertRedditMessage,
		})
	}
}
