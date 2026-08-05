package connector

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exmime"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

// aboutCacheTTL is how long a Reddit user's info is reused before being re-fetched. Avatars
// change rarely, and this keeps the cost of aggressive info updates to roughly one request per
// user per hour instead of one per message.
const aboutCacheTTL = 1 * time.Hour

type cachedAbout struct {
	about     *redditchat.UserAbout
	fetchedAt time.Time
	err       bool
}

type aboutCache struct {
	lock    sync.Mutex
	entries map[string]cachedAbout
	// uploads maps a Reddit avatar URL to the mxc:// URI it was uploaded to, so the same image
	// is fetched from Reddit and uploaded to Matrix at most once per bridge run.
	uploads map[string]id.ContentURIString
	// mediaBroken is set when the homeserver turns out not to serve /_matrix/media at all.
	// Some deployments disable the media repository or delegate it to a service that isn't
	// routed; there, every avatar upload fails identically and retrying only fills the log.
	mediaBroken bool
}

func newAboutCache() *aboutCache {
	return &aboutCache{
		entries: make(map[string]cachedAbout),
		uploads: make(map[string]id.ContentURIString),
	}
}

// uploadAvatar fetches an avatar from Reddit and uploads it to Matrix, returning the mxc URI.
//
// Doing the upload here rather than handing bridgev2 a Get function means a homeserver without a
// media repository is detected once and then skipped, instead of failing for every ghost on
// every resync.
func (ac *aboutCache) uploadAvatar(
	ctx context.Context, rc *RedditChatConnector, web *redditchat.TokenRefresher, avatarURL string,
) id.ContentURIString {
	ac.lock.Lock()
	if ac.mediaBroken {
		ac.lock.Unlock()
		return ""
	}
	if mxc, ok := ac.uploads[avatarURL]; ok {
		ac.lock.Unlock()
		return mxc
	}
	ac.lock.Unlock()

	log := zerolog.Ctx(ctx)
	data, err := web.DownloadWeb(ctx, avatarURL)
	if err != nil {
		log.Debug().Err(err).Str("url", avatarURL).Msg("Failed to download Reddit avatar")
		return ""
	}
	mime := http.DetectContentType(data)
	fileName := "avatar" + exmime.ExtensionFromMimetype(mime)
	var mxc id.ContentURIString
	if rc.Config.MatrixMediaURL != "" {
		mxc, err = rc.uploadViaMediaURL(ctx, data, fileName, mime)
	} else {
		mxc, _, err = rc.br.Bot.UploadMedia(ctx, "", data, fileName, mime)
	}
	if err != nil {
		if mautrix.MUnrecognized.Is(err) || mautrix.MNotFound.Is(err) {
			ac.lock.Lock()
			first := !ac.mediaBroken
			ac.mediaBroken = true
			ac.lock.Unlock()
			if first {
				log.Warn().Err(err).Msg(
					"Homeserver does not serve /_matrix/media, so avatars cannot be bridged. " +
						"Enable the media repository (or route it to whatever serves it) and restart the bridge.")
			}
			return ""
		}
		log.Debug().Err(err).Msg("Failed to upload Reddit avatar")
		return ""
	}
	ac.lock.Lock()
	ac.uploads[avatarURL] = mxc
	ac.lock.Unlock()
	return mxc
}

// get returns a user's Reddit info, fetching it at most once per TTL per user.
//
// Failures are cached too, with the same TTL, so a suspended or renamed account doesn't cause a
// request on every incoming message.
func (ac *aboutCache) get(ctx context.Context, web *redditchat.TokenRefresher, username string) *redditchat.UserAbout {
	ac.lock.Lock()
	entry, ok := ac.entries[username]
	ac.lock.Unlock()
	if ok && time.Since(entry.fetchedAt) < aboutCacheTTL {
		return entry.about
	}

	about, err := web.UserAbout(ctx, username)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Str("username", username).Msg("Failed to fetch Reddit user info")
	}
	ac.lock.Lock()
	ac.entries[username] = cachedAbout{about: about, fetchedAt: time.Now(), err: err != nil}
	ac.lock.Unlock()
	return about
}
