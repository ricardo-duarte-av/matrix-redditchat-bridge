package connector

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

// reactionNotice is what the bridge bot posts when someone reacts from Matrix.
//
// Reddit only accepts reaction keys from its own fixed emoji set and refuses anything else with
// M_INVALID_ARGUMENT_VALUE "reaction key is not supported", so no unicode reaction a Matrix
// client can produce is sendable. Saying so in the room beats the reaction silently vanishing.
const reactionNotice = "Reactions aren't supported on Reddit chat: it only accepts reactions from its own fixed emoji set, " +
	"so this reaction was not sent. Reactions made on Reddit are still bridged into Matrix."

var (
	_ bridgev2.ReactionHandlingNetworkAPI = (*RedditChatClient)(nil)
)

// PreHandleMatrixReaction is required by the interface. The reaction cannot be sent, so nothing
// is reserved here; the refusal happens in HandleMatrixReaction where a notice can be posted.
func (c *RedditChatClient) PreHandleMatrixReaction(
	_ context.Context, msg *bridgev2.MatrixReaction,
) (bridgev2.MatrixReactionPreResponse, error) {
	return bridgev2.MatrixReactionPreResponse{
		SenderID: networkid.UserID(c.UserLogin.ID),
		EmojiID:  networkid.EmojiID(msg.Content.RelatesTo.Key),
		Emoji:    msg.Content.RelatesTo.Key,
	}, nil
}

// HandleMatrixReaction refuses the reaction and explains why in the room.
//
// The notice is sent by the bridge bot into the portal, and is never relayed to Reddit: the
// bridge only forwards events from logged-in Matrix users, and the bot is in the appservice
// namespace.
func (c *RedditChatClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	c.noticeOnce(ctx, msg.Portal, reactionNotice)
	return nil, fmt.Errorf("%w: Reddit chat only accepts reactions from its own emoji set", bridgev2.ErrReactionsNotSupported)
}

// HandleMatrixReactionRemove is a no-op: nothing was ever sent to Reddit to remove.
func (c *RedditChatClient) HandleMatrixReactionRemove(_ context.Context, _ *bridgev2.MatrixReactionRemove) error {
	return nil
}

// noticeOnce posts an m.notice into the portal, at most once per portal per bridge run, so a
// user reacting repeatedly doesn't flood the room.
func (c *RedditChatClient) noticeOnce(ctx context.Context, portal *bridgev2.Portal, text string) {
	if portal == nil || portal.MXID == "" {
		return
	}
	c.noticeLock.Lock()
	if c.noticedPortals == nil {
		c.noticedPortals = make(map[id.RoomID]bool)
	}
	already := c.noticedPortals[portal.MXID]
	c.noticedPortals[portal.MXID] = true
	c.noticeLock.Unlock()
	if already {
		return
	}
	content := &event.Content{Parsed: &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    text,
	}}
	if _, err := c.Main.br.Bot.SendMessage(ctx, portal.MXID, event.EventMessage, content, nil); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to post the unsupported-reaction notice")
	}
}

// handleRedditReaction turns a Reddit reaction into a Matrix one.
//
// Reddit's reaction keys are emoji IDs served from its CDN (e.g. jvuspmbga7081.gif), not unicode,
// so the image is re-hosted on Matrix and attached to the reaction using the image-reaction
// fields clients look for. The key itself is kept as the emoji ID so that adds and removals
// still pair up.
func (c *RedditChatClient) handleRedditReaction(ctx context.Context, portalKey networkid.PortalKey, evt *event.Event, remove bool) {
	log := zerolog.Ctx(ctx)
	if err := redditchat.ParseContent(evt, event.EventReaction); err != nil {
		log.Debug().Err(err).Stringer("event_id", evt.ID).Msg("Failed to parse Reddit reaction")
		return
	}
	target, key, ok := redditReactionTarget(evt)
	if !ok {
		return
	}

	evtType := bridgev2.RemoteEventReaction
	if remove {
		evtType = bridgev2.RemoteEventReactionRemove
	}
	reaction := &simplevent.Reaction{
		EventMeta: simplevent.EventMeta{
			Type:      evtType,
			PortalKey: portalKey,
			Sender:    c.senderOf(evt.Sender),
			Timestamp: time.UnixMilli(evt.Timestamp),
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Str("reaction_key", key)
			},
		},
		TargetMessage: networkid.MessageID(target),
		EmojiID:       networkid.EmojiID(key),
		Emoji:         key,
	}
	if !remove {
		if mxc, info := c.rehostReactionImage(ctx, key); mxc != "" {
			// Clients that render image reactions look for these; the ones that don't fall back
			// to the shortcode text.
			reaction.Emoji = reactionShortcode(key)
			reaction.ExtraContent = map[string]any{
				"com.beeper.reaction.shortcode": reaction.Emoji,
				"shortcode":                     reaction.Emoji,
				"url":                           string(mxc),
				"info":                          info,
			}
		}
	}
	c.Main.br.QueueRemoteEvent(c.UserLogin, reaction)
}

// rehostReactionImage uploads a Reddit reaction emoji to Matrix, reusing the avatar upload cache
// so the same emoji is fetched and uploaded at most once per bridge run.
func (c *RedditChatClient) rehostReactionImage(ctx context.Context, key string) (id.ContentURIString, map[string]any) {
	imageURL := redditEmojiURL(key)
	if imageURL == "" {
		return "", nil
	}
	mxc := c.Main.aboutCache.uploadAvatar(ctx, c.Main, c.web(), imageURL)
	if mxc == "" {
		return "", nil
	}
	return mxc, map[string]any{"mimetype": mimeForKey(key)}
}

// redditEmojiURL maps a reaction key onto Reddit's CDN. Keys carry their own extension, e.g.
// jvuspmbga7081.gif.
func redditEmojiURL(key string) string {
	if key == "" || strings.ContainsAny(key, "/ ") || !strings.Contains(key, ".") {
		return ""
	}
	return "https://i.redd.it/" + key
}

func mimeForKey(key string) string {
	switch {
	case strings.HasSuffix(key, ".gif"):
		return "image/gif"
	case strings.HasSuffix(key, ".png"):
		return "image/png"
	case strings.HasSuffix(key, ".jpg"), strings.HasSuffix(key, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(key, ".webp"):
		return "image/webp"
	default:
		return http.DetectContentType(nil)
	}
}

// Removing a reaction is deliberately not implemented yet. bridgev2 resolves a removal by
// (target message, sender, emoji ID), but a Reddit redaction only carries the reaction's own
// event ID, and it has not been confirmed that Reddit even removes reactions by redacting.
// Guessing here would produce a handler that silently never matches anything.

// redditReactionTarget extracts the target event and emoji key from a Reddit reaction.
//
// Reddit delivers reactions in the standard nested form, but its own client *sends* them
// flattened (event_id/key/rel_type at the top level), so both shapes are accepted here rather
// than relying on the parsed struct alone.
func redditReactionTarget(evt *event.Event) (target, key string, ok bool) {
	if rel := evt.Content.AsReaction().RelatesTo; rel.EventID != "" && rel.Key != "" {
		return rel.EventID.String(), rel.Key, true
	}
	raw := evt.Content.Raw
	if raw == nil {
		return "", "", false
	}
	target, _ = raw["event_id"].(string)
	key, _ = raw["key"].(string)
	return target, key, target != "" && key != ""
}

// reactionShortcode turns a Reddit emoji ID into something readable for clients that don't
// render image reactions, e.g. jvuspmbga7081.gif -> :jvuspmbga7081:
func reactionShortcode(key string) string {
	name := key
	if idx := strings.LastIndexByte(name, '.'); idx > 0 {
		name = name[:idx]
	}
	return ":" + name + ":"
}
