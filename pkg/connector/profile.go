package connector

import (
	"context"
	"sync"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

// profileStore remembers the latest com.reddit.profile seen for each user.
//
// These events are the only in-band source of avatars: Reddit's /profile endpoint and its member
// events both return an empty avatar_url. Keeping them means avatars work for logins that have
// no session cookie, and update themselves when someone changes their avatar, instead of the
// bridge scraping reddit.com.
type profileStore struct {
	lock       sync.RWMutex
	byUser     map[networkid.UserID]*redditchat.Profile
	byUsername map[string]*redditchat.Profile
}

func newProfileStore() *profileStore {
	return &profileStore{
		byUser:     make(map[networkid.UserID]*redditchat.Profile),
		byUsername: make(map[string]*redditchat.Profile),
	}
}

func (ps *profileStore) put(userID networkid.UserID, p *redditchat.Profile) {
	ps.lock.Lock()
	defer ps.lock.Unlock()
	if userID != "" {
		ps.byUser[userID] = p
	}
	if name := p.Name(); name != "" {
		ps.byUsername[name] = p
	}
}

// get looks a profile up by user ID, falling back to the username, since a profile event carries
// no user ID and can only be attributed when the message it relates to is already bridged.
func (ps *profileStore) get(userID networkid.UserID, username string) *redditchat.Profile {
	ps.lock.RLock()
	defer ps.lock.RUnlock()
	if p, ok := ps.byUser[userID]; ok {
		return p
	}
	if username != "" {
		if p, ok := ps.byUsername[username]; ok {
			return p
		}
	}
	return nil
}

// handleRedditProfile records a profile event and refreshes the ghost it belongs to.
//
// The event names no user, so the owner is resolved through the message it relates to. When that
// message isn't bridged (it may predate the bridge, or be in a room that was skipped), the
// profile is still stored under its username so [profileStore.get] can find it later.
func (c *RedditChatClient) handleRedditProfile(ctx context.Context, evt *event.Event) {
	log := zerolog.Ctx(ctx)
	profile, ok := redditchat.ParseProfileEvent(evt)
	if !ok {
		return
	}

	var userID networkid.UserID
	if profile.RelatesToEventID != "" {
		msg, err := c.Main.br.DB.Message.GetLastPartByID(
			ctx, c.UserLogin.ID, networkid.MessageID(profile.RelatesToEventID))
		if err != nil {
			log.Debug().Err(err).Msg("Failed to look up the message a profile relates to")
		} else if msg != nil {
			userID = msg.SenderID
		}
	}
	c.Main.profiles.put(userID, profile)
	if userID == "" {
		return
	}

	ghost, err := c.Main.br.GetGhostByID(ctx, userID)
	if err != nil {
		log.Debug().Err(err).Str("user_id", string(userID)).Msg("Failed to get ghost for profile update")
		return
	}
	ghost.UpdateInfo(ctx, c.userInfoFromProfile(ctx, userID, profile))
}

// userInfoFromProfile builds ghost info from a Reddit profile event.
func (c *RedditChatClient) userInfoFromProfile(ctx context.Context, userID networkid.UserID, profile *redditchat.Profile) *bridgev2.UserInfo {
	name := profile.Name()
	info := &bridgev2.UserInfo{
		Name:        &name,
		Identifiers: []string{"reddit:" + string(userID)},
	}
	if avatarURL := profile.AvatarURL(); avatarURL != "" {
		if mxc := c.Main.aboutCache.uploadAvatar(ctx, c.Main, c.web(), avatarURL); mxc != "" {
			info.Avatar = &bridgev2.Avatar{
				ID:  networkid.AvatarID(avatarURL),
				MXC: mxc,
			}
		}
	}
	return info
}
