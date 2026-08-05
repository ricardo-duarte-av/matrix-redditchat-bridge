package connector

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

// FetchMessages paginates a Reddit room's timeline. Reddit's /messages endpoint is the standard
// Matrix one, so its pagination tokens are used directly as bridgev2 pagination cursors.
func (c *RedditChatClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	log := zerolog.Ctx(ctx)
	roomID := id.RoomID(params.Portal.ID)

	dir := mautrix.DirectionBackward
	from := string(params.Cursor)
	if params.Forward {
		dir = mautrix.DirectionForward
		// Forward backfill resumes from the newest message the bridge already knows about.
		if from == "" && params.AnchorMessage != nil {
			from = string(params.AnchorMessage.ID)
		}
	}

	count := params.Count
	if count <= 0 {
		count = c.Main.Config.BackfillBatchSize
	}

	resp, err := c.Client.Messages(ctx, roomID, from, dir, count)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Reddit messages: %w", err)
	}

	messages := make([]*bridgev2.BackfillMessage, 0, len(resp.Chunk))
	for _, evt := range resp.Chunk {
		msg := c.convertBackfillEvent(ctx, evt)
		if msg != nil {
			messages = append(messages, msg)
		}
	}
	// Backward pagination returns newest-first, but bridgev2 wants chronological order.
	if dir == mautrix.DirectionBackward {
		slices.Reverse(messages)
	}
	log.Debug().
		Int("chunk_size", len(resp.Chunk)).
		Int("bridged", len(messages)).
		Bool("forward", params.Forward).
		Msg("Fetched Reddit history batch")

	return &bridgev2.FetchMessagesResponse{
		Messages: messages,
		Cursor:   networkid.PaginationCursor(resp.End),
		// Reddit marks the start of a room's history with a t0_0 end token; paginating from it
		// returns empty chunks forever, so it terminates the walk. A chunk containing only
		// unsupported event types still means there is more to fetch.
		HasMore: redditchat.HasMoreHistory(resp),
		Forward: params.Forward,
	}, nil
}

func (c *RedditChatClient) convertBackfillEvent(ctx context.Context, evt *event.Event) *bridgev2.BackfillMessage {
	if evt.Type != event.EventMessage {
		return nil
	}
	if err := redditchat.ParseContent(evt, evt.Type); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Stringer("event_id", evt.ID).Msg("Failed to parse Reddit message during backfill")
		return nil
	}
	content := evt.Content.AsMessage()
	if content.MsgType != event.MsgText && content.MsgType != event.MsgNotice && content.MsgType != event.MsgEmote {
		return nil
	}
	converted, err := convertRedditMessage(ctx, nil, nil, content)
	if err != nil {
		return nil
	}
	return &bridgev2.BackfillMessage{
		ConvertedMessage: converted,
		Sender:           c.senderOf(evt.Sender),
		ID:               networkid.MessageID(evt.ID),
		Timestamp:        time.UnixMilli(evt.Timestamp),
	}
}
