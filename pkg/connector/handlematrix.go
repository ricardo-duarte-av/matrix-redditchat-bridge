package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

// HandleMatrixAcceptMessageRequest joins the Reddit room, which is how a chat request is
// accepted. bridgev2 calls this either explicitly, or implicitly when the user replies in a
// portal still marked as a message request.
//
// This is the only place the bridge joins a Reddit room, and it is always a consequence of the
// user acting: a join is visible to whoever started the chat, so it must never happen just
// because the bridge synced.
func (c *RedditChatClient) HandleMatrixAcceptMessageRequest(ctx context.Context, msg *bridgev2.MatrixAcceptMessageRequest) error {
	roomID := id.RoomID(msg.Portal.ID)
	if err := c.Client.JoinRoom(ctx, roomID); err != nil {
		return fmt.Errorf("failed to accept Reddit chat request: %w", err)
	}
	zerolog.Ctx(ctx).Info().
		Stringer("room_id", roomID).
		Bool("implicit", msg.Content != nil && msg.Content.IsImplicit).
		Msg("Accepted Reddit chat request")
	return nil
}

// HandleMatrixMessage sends a Matrix message to Reddit. Reddit echoes it back through /sync,
// but the message is stored under its Reddit event ID here, and the echo carries the same ID,
// so bridgev2's own duplicate check drops it.
func (c *RedditChatClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	switch msg.Content.MsgType {
	case event.MsgText, event.MsgNotice, event.MsgEmote:
	default:
		return nil, fmt.Errorf("%w: %s messages are not supported yet", bridgev2.ErrUnsupportedMessageType, msg.Content.MsgType)
	}

	content := *msg.Content
	// Replies aren't bridged yet, and forwarding a relation pointing at a Matrix event ID would
	// produce a dangling reference on the Reddit side.
	content.RelatesTo = nil
	content.NewContent = nil

	eventID, err := c.Client.SendText(ctx, id.RoomID(msg.Portal.ID), &content, string(msg.InputTransactionID))
	if err != nil {
		if redditchat.IsTokenError(err) {
			c.reportError(err, "failed to send message")
		}
		return nil, fmt.Errorf("failed to send message to Reddit: %w", err)
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        networkid.MessageID(eventID),
			SenderID:  networkid.UserID(c.UserLogin.ID),
			Timestamp: time.UnixMilli(msg.Event.Timestamp),
			SendTxnID: msg.InputTransactionID,
		},
	}, nil
}
