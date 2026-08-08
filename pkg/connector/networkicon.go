package connector

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/id"
)

// networkIconPNG is the Reddit icon shown as the protocol avatar in m.bridge state events.
//
// Official mautrix bridges point at an image already uploaded to maunium.net, which isn't an
// option here, so the image ships with the bridge and is uploaded to the local homeserver on
// startup.
//
//go:embed assets/network-icon.png
var networkIconPNG []byte

// networkIconKey stores "<sha256 of the image> <mxc uri>" so the upload happens once per
// homeserver rather than once per start, while still re-uploading if the image is replaced.
const networkIconKey database.Key = "redditchat_network_icon"

const networkIconMime = "image/png"

// ensureNetworkIcon uploads the embedded network icon if it hasn't been uploaded yet, and makes
// the resulting mxc URI available to GetName.
//
// A failure here is not fatal: the bridge simply keeps working without a protocol avatar, and
// the upload is retried on the next start.
func (rc *RedditChatConnector) ensureNetworkIcon(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	sum := sha256.Sum256(networkIconPNG)
	hash := hex.EncodeToString(sum[:])

	storedHash, storedMXC, _ := strings.Cut(rc.br.DB.KV.Get(ctx, networkIconKey), " ")
	if storedHash == hash && storedMXC != "" {
		rc.networkIcon.Store(storedMXC)
		return
	}

	var mxc id.ContentURIString
	var err error
	if rc.Config.MatrixMediaURL != "" {
		mxc, err = rc.uploadViaMediaURL(ctx, networkIconPNG, "reddit-chat.png", networkIconMime)
	} else {
		mxc, _, err = rc.br.Bot.UploadMedia(ctx, "", networkIconPNG, "reddit-chat.png", networkIconMime)
	}
	if err != nil {
		log.Warn().Err(err).Msg("Failed to upload the network icon, bridge info will have no protocol avatar")
		if storedMXC != "" {
			// Keep showing the previous upload rather than dropping the avatar entirely.
			rc.networkIcon.Store(storedMXC)
		}
		return
	}

	rc.br.DB.KV.Set(ctx, networkIconKey, hash+" "+string(mxc))
	rc.networkIcon.Store(string(mxc))
	log.Info().Str("mxc", string(mxc)).Msg("Uploaded the network icon for bridge info")
	if storedMXC != string(mxc) {
		rc.invalidateBridgeInfoVersion(ctx)
	}
}

// networkIconMXC returns the uploaded icon, or an empty string before the upload has happened.
func (rc *RedditChatConnector) networkIconMXC() id.ContentURIString {
	mxc, _ := rc.networkIcon.Load().(string)
	return id.ContentURIString(mxc)
}

// invalidateBridgeInfoVersion makes bridgev2's PostStart think the bridge info version changed,
// so every existing portal gets its m.bridge event resent with the new protocol avatar.
//
// PostStart runs after Start, so writing the key here is enough; the capability version is left
// alone to avoid resending capabilities that didn't change.
func (rc *RedditChatConnector) invalidateBridgeInfoVersion(ctx context.Context) {
	raw := rc.br.DB.KV.Get(ctx, database.KeyBridgeInfoVersion)
	if raw == "" {
		// First start: PostStart will resend everything anyway (and there are no portals yet).
		return
	}
	var infoVer, capVer int
	if _, err := fmt.Sscanf(raw, "%d,%d", &infoVer, &capVer); err != nil {
		return
	}
	expectedInfoVer, _ := rc.GetBridgeInfoVersion()
	if infoVer == expectedInfoVer {
		rc.br.DB.KV.Set(ctx, database.KeyBridgeInfoVersion, fmt.Sprintf("%d,%d", expectedInfoVer-1, capVer))
	}
}
