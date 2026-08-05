package connector

import (
	"context"
	"fmt"
	"net/http"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exmime"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

// mediaMsgTypes are the message types carrying a file. Reddit only ever sends m.image in
// practice, but the others cost nothing to accept and are handled identically.
var mediaMsgTypes = map[event.MessageType]bool{
	event.MsgImage: true,
	event.MsgVideo: true,
	event.MsgAudio: true,
	event.MsgFile:  true,
}

// convertRedditMedia re-hosts a Reddit media message on the Matrix side.
//
// Reddit's mxc:// URIs only resolve against Reddit's server, so a Matrix client could never load
// them: the bytes have to be fetched from Reddit and re-uploaded to the local homeserver. Reddit
// answers the download with a 308 to its public CDN, which [redditchat.Client.DownloadMedia]
// follows.
// mediaConverter binds the event's raw content so Reddit-only fields (which the parsed struct
// drops) can still be inspected during conversion.
func (c *RedditChatClient) mediaConverter(raw map[string]any) func(
	context.Context, *bridgev2.Portal, bridgev2.MatrixAPI, *event.MessageEventContent,
) (*bridgev2.ConvertedMessage, error) {
	return func(
		ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, content *event.MessageEventContent,
	) (*bridgev2.ConvertedMessage, error) {
		return c.convertRedditMedia(ctx, portal, intent, content, raw)
	}
}

func (c *RedditChatClient) convertRedditMedia(
	ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI,
	content *event.MessageEventContent, raw map[string]any,
) (*bridgev2.ConvertedMessage, error) {
	log := zerolog.Ctx(ctx)
	uri, err := content.URL.Parse()
	if err != nil || uri.IsEmpty() {
		return nil, fmt.Errorf("media message has no usable URL: %w", err)
	}

	data, contentType, err := c.Client.DownloadMedia(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("failed to download media from Reddit: %w", err)
	}

	cloned := *content
	cloned.RelatesTo = nil
	cloned.NewContent = nil
	if cloned.Info == nil {
		cloned.Info = &event.FileInfo{}
	} else {
		infoCopy := *cloned.Info
		cloned.Info = &infoCopy
	}
	// Trust the bytes over Reddit's metadata: the declared mimetype is sometimes absent, and the
	// size it reports is not always the size of what the CDN actually returns.
	if cloned.Info.MimeType == "" {
		cloned.Info.MimeType = contentType
	}
	if cloned.Info.MimeType == "" {
		cloned.Info.MimeType = http.DetectContentType(data)
	}
	cloned.Info.Size = len(data)
	if cloned.Body == "" || cloned.Body == "Image" {
		cloned.Body = mediaFileName(cloned.Info.MimeType, cloned.MsgType)
	}
	cloned.FileName = cloned.Body

	mxc, encrypted, err := intent.UploadMedia(ctx, portal.MXID, data, cloned.Body, cloned.Info.MimeType)
	if err != nil {
		return nil, fmt.Errorf("failed to reupload media to Matrix: %w", err)
	}
	cloned.URL = mxc
	cloned.File = encrypted

	// Reddit's own fields point at URIs only its server can resolve, so they would be dead links
	// in a Matrix client.
	if cloned.Info.ThumbnailURL != "" {
		cloned.Info.ThumbnailURL = ""
		cloned.Info.ThumbnailInfo = nil
	}
	// Carry Reddit's NSFW marker through so clients that understand it can act on it.
	extra := map[string]any{}
	if nsfw, ok := raw[redditchat.NSFWImageField].(bool); ok && nsfw {
		extra[redditchat.NSFWImageField] = true
	}

	log.Debug().
		Str("mimetype", cloned.Info.MimeType).
		Int("size", len(data)).
		Msg("Re-hosted Reddit media on Matrix")

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: &cloned,
			Extra:   extra,
		}},
	}, nil
}

// mediaFileName invents a filename, since Reddit sends every image with the body "Image".
func mediaFileName(mimeType string, msgType event.MessageType) string {
	base := "file"
	switch msgType {
	case event.MsgImage:
		base = "image"
	case event.MsgVideo:
		base = "video"
	case event.MsgAudio:
		base = "audio"
	}
	return base + extensionFor(mimeType)
}

func extensionFor(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "audio/mpeg":
		return ".mp3"
	default:
		return exmime.ExtensionFromMimetype(mimeType)
	}
}
