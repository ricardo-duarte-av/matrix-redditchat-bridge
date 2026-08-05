package connector

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strings"
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
	switch {
	case msg.Content.MsgType == event.MsgText || msg.Content.MsgType == event.MsgNotice || msg.Content.MsgType == event.MsgEmote:
	case mediaMsgTypes[msg.Content.MsgType]:
		return c.sendMatrixMedia(ctx, msg)
	default:
		return nil, fmt.Errorf("%w: %s messages are not supported yet", bridgev2.ErrUnsupportedMessageType, msg.Content.MsgType)
	}

	content := *msg.Content
	content.NewContent = nil
	// Rebuild the relation against Reddit's event IDs. Forwarding the Matrix relation as-is
	// would point at Matrix event IDs, which mean nothing to Reddit.
	content.RelatesTo = redditRelation(msg)

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

// sendMatrixMedia re-hosts a Matrix file on Reddit and sends it as an image message.
//
// Reddit's media endpoint accepts images only and validates the bytes rather than trusting the
// declared type, so anything else is rejected up front with an error the user can act on instead
// of a confusing failure from Reddit. The event shape matches what Reddit's own web client
// sends: body "Image", and info carrying w/h/mimetype/size.
func (c *RedditChatClient) sendMatrixMedia(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	content := msg.Content
	data, err := c.Main.br.Bot.DownloadMedia(ctx, content.URL, content.File)
	if err != nil {
		return nil, fmt.Errorf("failed to download media from Matrix: %w", err)
	}

	mimeType := ""
	if content.Info != nil {
		mimeType = content.Info.MimeType
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	// Normalise away any parameters such as "image/jpeg; charset=binary".
	if idx := strings.IndexByte(mimeType, ';'); idx >= 0 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}

	if !redditchat.SupportedUploadTypes[mimeType] {
		return nil, fmt.Errorf("%w: Reddit chat only accepts images (jpeg, png, gif, webp), not %s",
			bridgev2.ErrUnsupportedMessageType, mimeType)
	}
	if limit := redditchat.UploadLimitFor(mimeType); len(data) > limit {
		return nil, fmt.Errorf("%w: %.1f MB exceeds Reddit's %d MB limit for %s",
			bridgev2.ErrMediaDownloadFailed, float64(len(data))/(1<<20), limit>>20, mimeType)
	}

	fileName := content.FileName
	if fileName == "" {
		fileName = content.Body
	}
	if fileName == "" {
		fileName = "image"
	}
	redditURL, err := c.Client.UploadMedia(ctx, data, fileName, mimeType)
	if err != nil {
		return nil, fmt.Errorf("failed to upload media to Reddit: %w", err)
	}

	width, height := 0, 0
	if content.Info != nil {
		width, height = content.Info.Width, content.Info.Height
	}
	if width == 0 || height == 0 {
		if cfg, _, decodeErr := image.DecodeConfig(bytes.NewReader(data)); decodeErr == nil {
			width, height = cfg.Width, cfg.Height
		}
	}

	// Match Reddit's own client exactly; its UI keys off info rather than body.
	outgoing := &event.MessageEventContent{
		MsgType:   event.MsgImage,
		Body:      "Image",
		URL:       redditURL,
		RelatesTo: redditRelation(msg),
		Info: &event.FileInfo{
			Width:    width,
			Height:   height,
			MimeType: mimeType,
			Size:     len(data),
		},
	}
	eventID, err := c.Client.SendText(ctx, id.RoomID(msg.Portal.ID), outgoing, string(msg.InputTransactionID))
	if err != nil {
		if redditchat.IsTokenError(err) {
			c.reportError(err, "failed to send media")
		}
		return nil, fmt.Errorf("failed to send media message to Reddit: %w", err)
	}
	zerolog.Ctx(ctx).Debug().
		Str("mimetype", mimeType).Int("size", len(data)).
		Msg("Uploaded Matrix media to Reddit")

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        networkid.MessageID(eventID),
			SenderID:  networkid.UserID(c.UserLogin.ID),
			Timestamp: time.UnixMilli(msg.Event.Timestamp),
			SendTxnID: msg.InputTransactionID,
		},
	}, nil
}

// redditRelation builds the relation to send to Reddit for a Matrix message.
//
// Reddit models replies as threads: its client sends rel_type m.thread with an m.in_reply_to
// fallback, and that is how replies show up in its UI. A Matrix reply or thread reply is
// therefore both mapped onto a Reddit thread, keyed by the Reddit event ID the bridge stored
// for the target rather than the Matrix one.
func redditRelation(msg *bridgev2.MatrixMessage) *event.RelatesTo {
	target := msg.ThreadRoot
	if target == nil {
		target = msg.ReplyTo
	}
	if target == nil || target.ID == "" {
		return nil
	}
	rootID := id.EventID(target.ID)
	rel := &event.RelatesTo{
		Type:    event.RelThread,
		EventID: rootID,
		// Reddit's own client sets the reply fallback, and older clients rely on it.
		InReplyTo:     &event.InReplyTo{EventID: rootID},
		IsFallingBack: true,
	}
	// When replying to a specific message inside a thread, point the fallback at that message.
	if msg.ReplyTo != nil && msg.ReplyTo.ID != "" && msg.ThreadRoot != nil {
		rel.InReplyTo.EventID = id.EventID(msg.ReplyTo.ID)
		rel.IsFallingBack = false
	}
	return rel
}
