package connector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

const (
	syncMinBackoff = 2 * time.Second
	syncMaxBackoff = 2 * time.Minute
	// How long before a token's expiry to nag the user about re-logging in.
	tokenExpiryWarning = 30 * time.Minute
)

// syncLoop drives Reddit's /sync endpoint. The next_batch token is persisted after every
// successful batch so a restart resumes instead of replaying the whole timeline.
func (c *RedditChatClient) syncLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	log := c.UserLogin.Log.With().Str("component", "reddit sync").Logger()
	ctx = log.WithContext(ctx)
	backoff := syncMinBackoff

	var warnedExpiry, reconciled bool
	for {
		if ctx.Err() != nil {
			return
		}
		// Reddit chat tokens last 24 hours. If a session cookie is stored the bridge mints a
		// new one itself; otherwise all it can do is tell the user, with a real deadline,
		// before the bridge goes dark.
		if refreshed, err := c.ensureFreshToken(ctx, false); err != nil {
			log.Err(err).Msg("Failed to refresh Reddit chat token")
		} else if refreshed {
			warnedExpiry = false
		}
		if expiry := c.meta().TokenExpiresAt; !expiry.IsZero() {
			if time.Now().After(expiry) {
				log.Warn().Time("expired_at", expiry).Msg("Reddit token expired")
				c.loggedIn.Store(false)
				c.UserLogin.BridgeState.Send(status.BridgeState{
					StateEvent: status.StateBadCredentials,
					Error:      "redditchat-token-expired",
					Message:    c.expiredMessage(expiry),
				})
				return
			} else if !warnedExpiry && !c.CanRefresh() && time.Until(expiry) < tokenExpiryWarning {
				warnedExpiry = true
				c.UserLogin.BridgeState.Send(status.BridgeState{
					StateEvent: status.StateTransientDisconnect,
					Error:      "redditchat-token-expiring",
					Message: fmt.Sprintf(
						"Your Reddit chat token expires at %s. Run `login` again with a fresh token to avoid an interruption.",
						expiry.Format(time.RFC1123)),
				})
			}
		}

		resp, err := c.Client.Sync(ctx, c.meta().NextBatch, c.Main.Config.SyncTimeout)
		if err != nil && redditchat.IsTokenError(err) && c.CanRefresh() {
			// Reddit rejected the token earlier than its exp claim said it would. Force a
			// refresh and retry once before treating this as a real failure.
			log.Warn().Msg("Reddit rejected the chat token, forcing a refresh")
			if _, refreshErr := c.ensureFreshToken(ctx, true); refreshErr != nil {
				log.Err(refreshErr).Msg("Forced token refresh failed")
			} else {
				resp, err = c.Client.Sync(ctx, c.meta().NextBatch, c.Main.Config.SyncTimeout)
			}
		}
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			log.Err(err).Dur("retry_in", backoff).Msg("Sync failed")
			c.reportError(err, "Reddit sync failed")
			if redditIsFatal(err) && !c.CanRefresh() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, syncMaxBackoff)
			continue
		}
		if backoff != syncMinBackoff {
			backoff = syncMinBackoff
			c.loggedIn.Store(true)
			c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
		}

		c.handleSync(ctx, resp)

		// Reddit's sync room list is capped, so after the first batch reconcile against the
		// full joined-room list to pick up anything it left out.
		if !reconciled {
			reconciled = true
			c.reconcileJoinedRooms(ctx, syncedRoomSet(resp))
		}

		c.meta().NextBatch = resp.NextBatch
		if err = c.UserLogin.Save(ctx); err != nil {
			log.Err(err).Msg("Failed to save sync token")
		}
	}
}

// redditIsFatal reports whether retrying the sync is pointless. Only a dead token qualifies:
// everything else (network blips, 5xx) is worth backing off and retrying.
func redditIsFatal(err error) bool {
	return redditchat.IsTokenError(err)
}

func (c *RedditChatClient) handleSync(ctx context.Context, resp *mautrix.RespSync) {
	log := zerolog.Ctx(ctx)

	// `invite` membership on Reddit is not a Matrix-style invite: Reddit has no invite concept
	// in its UI. It just means someone started a chat and the user has never replied, which is
	// Reddit's chat-request state. The room is fully readable already, so the portal is created
	// and marked as a message request. Joining is deliberately NOT done here - a join is
	// visible to the other user, and doing it automatically would silently accept every pending
	// request on the account. Replying accepts it instead, via HandleMatrixAcceptMessageRequest.
	for roomID, invited := range resp.Rooms.Invite {
		if skip, reason := c.shouldSkipRequest(ctx, roomID); skip {
			log.Debug().Stringer("room_id", roomID).Str("reason", reason).Msg("Not bridging Reddit chat request")
			continue
		}
		log.Debug().Stringer("room_id", roomID).Msg("Bridging Reddit chat request without joining")
		// Reddit refuses /members and /state for unaccepted chats, so the chat info has to come
		// from the invite_state the sync response already carries.
		c.Main.br.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    c.portalKey(roomID),
				CreatePortal: true,
			},
			ChatInfo: c.chatInfoFromInviteState(ctx, invited.State.Events),
		})
	}

	for roomID, room := range resp.Rooms.Join {
		c.handleJoinedRoom(ctx, roomID, room)
	}
}

// shouldSkipRequest decides whether an unaccepted Reddit chat should be bridged at all.
//
// Reddit shows a chat request only when it is neither dismissed nor classified as spam, and the
// bridge matches that: a dismissed chat is one the user explicitly got rid of, and re-creating it
// as a Matrix room would undo that. The checks cost one request each, but only for chats that
// aren't already bridged, and sync only reports an unaccepted chat when something about it
// changes.
func (c *RedditChatClient) shouldSkipRequest(ctx context.Context, roomID id.RoomID) (bool, string) {
	if portal, err := c.Main.br.GetExistingPortalByKey(ctx, c.portalKey(roomID)); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Stringer("room_id", roomID).Msg("Failed to look up existing portal")
	} else if portal != nil {
		// Already bridged: leave it alone, the user may be using the portal.
		return false, ""
	}
	if !c.Main.Config.BridgeHiddenChats {
		hidden, err := c.Client.IsHiddenChat(ctx, roomID)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Stringer("room_id", roomID).Msg("Failed to check if chat is hidden")
		} else if hidden {
			return true, "dismissed on Reddit"
		}
	}
	if !c.Main.Config.BridgeSpamChats {
		spam, err := c.Client.IsSpamChat(ctx, roomID)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Stringer("room_id", roomID).Msg("Failed to check chat spam status")
		} else if spam {
			return true, "classified as spam by Reddit"
		}
	}
	return false, ""
}

func (c *RedditChatClient) handleJoinedRoom(ctx context.Context, roomID id.RoomID, room *mautrix.SyncJoinedRoom) {
	// A chat dismissed after being accepted is hidden on Reddit too. Its account data arrives in
	// sync, so this costs no extra request.
	if !c.Main.Config.BridgeHiddenChats && redditchat.IsHiddenChatInSync(room.AccountData.Events) {
		portal, err := c.Main.br.GetExistingPortalByKey(ctx, c.portalKey(roomID))
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Stringer("room_id", roomID).Msg("Failed to look up existing portal")
		} else if portal == nil {
			zerolog.Ctx(ctx).Debug().Stringer("room_id", roomID).Msg("Not bridging chat dismissed on Reddit")
			return
		}
	}
	portalKey := c.portalKey(roomID)

	// State changes outside the timeline (name/topic/avatar/membership) are handled by simply
	// resyncing the whole chat, which is cheap enough for Reddit's small chat rooms and avoids
	// having to replicate Matrix state resolution here.
	if len(room.State.Events) > 0 {
		c.Main.br.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    portalKey,
				CreatePortal: true,
			},
			GetChatInfoFunc: c.getChatInfo,
		})
	}

	for _, evt := range room.Timeline.Events {
		c.handleTimelineEvent(ctx, portalKey, evt)
	}
}

func (c *RedditChatClient) handleTimelineEvent(ctx context.Context, portalKey networkid.PortalKey, evt *event.Event) {
	log := zerolog.Ctx(ctx)
	switch evt.Type {
	case event.EventMessage:
		if err := redditchat.ParseContent(evt, evt.Type); err != nil {
			log.Warn().Err(err).Stringer("event_id", evt.ID).Msg("Failed to parse Reddit message")
			return
		}
		content := evt.Content.AsMessage()
		convert := convertRedditMessage
		switch {
		case content.MsgType == event.MsgText || content.MsgType == event.MsgNotice || content.MsgType == event.MsgEmote:
		case mediaMsgTypes[content.MsgType]:
			convert = c.mediaConverter(evt.Content.Raw)
		default:
			log.Debug().
				Stringer("event_id", evt.ID).
				Str("msgtype", string(content.MsgType)).
				Msg("Ignoring unsupported Reddit message type")
			return
		}
		c.Main.br.QueueRemoteEvent(c.UserLogin, &simplevent.Message[*event.MessageEventContent]{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventMessage,
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       c.senderOf(evt.Sender),
				Timestamp:    time.UnixMilli(evt.Timestamp),
				LogContext: func(c zerolog.Context) zerolog.Context {
					return c.Stringer("reddit_event_id", evt.ID)
				},
			},
			ID:                 networkid.MessageID(evt.ID),
			Data:               content,
			ConvertMessageFunc: convert,
		})
	case event.StateMember, event.StateRoomName, event.StateTopic, event.StateRoomAvatar:
		c.Main.br.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       c.senderOf(evt.Sender),
				Timestamp:    time.UnixMilli(evt.Timestamp),
			},
			GetChatInfoFunc: c.getChatInfo,
		})
	default:
		log.Debug().Str("type", evt.Type.Type).Msg("Ignoring unsupported Reddit event")
	}
}

// convertRedditMessage turns a Reddit m.room.message into a Matrix one. Both sides speak the
// same event format, so this is mostly a passthrough with the relation fields stripped, since
// replies and edits aren't bridged in v1 and dangling relations would confuse Matrix clients.
func convertRedditMessage(
	_ context.Context, _ *bridgev2.Portal, _ bridgev2.MatrixAPI, content *event.MessageEventContent,
) (*bridgev2.ConvertedMessage, error) {
	cloned := *content
	cloned.RelatesTo = nil
	cloned.NewContent = nil
	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: &cloned,
		}},
	}, nil
}
