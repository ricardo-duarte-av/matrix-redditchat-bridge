package redditchat

import (
	"strings"

	"maunium.net/go/mautrix/event"
)

// ProfileEventType is Reddit's own profile event. It rides along in the room timeline, sent by
// Reddit's system account, and carries the display name and avatar of the user who sent the
// message it relates to.
//
// This is the only place avatars exist inside Reddit's Matrix layer: profiles served from
// /profile and member events both have an empty avatar_url. Note that these events are withheld
// from any filtered sync, like reactions, so they only arrive when no filter is sent.
const ProfileEventType = "com.reddit.profile"

// Profile is the useful content of a com.reddit.profile event.
type Profile struct {
	// Username is the Reddit username, which is also what Reddit uses as the display name.
	Username string `json:"username"`
	// DisplayName is sometimes present and sometimes not; Username is the reliable one.
	DisplayName string `json:"displayname"`
	// IconURL is the avatar headshot. Always present, falling back to a Reddit default avatar.
	IconURL string `json:"icon_url"`
	// SnoovatarURL is the full-body avatar, absent for accounts that never made one.
	SnoovatarURL string `json:"snoovatar_url"`
	IsNSFW       bool   `json:"is_nsfw"`

	// RelatesToEventID is the message whose sender this profile describes. It is how a profile
	// is attributed to a user, since the event itself carries no user ID.
	RelatesToEventID string `json:"-"`
}

// Name returns the best display name available.
func (p *Profile) Name() string {
	if p.Username != "" {
		return p.Username
	}
	return p.DisplayName
}

// AvatarURL picks the image to use and strips the query string, so the value is stable enough to
// use as a change-detection key. Reddit regenerates the signed query on every fetch.
func (p *Profile) AvatarURL() string {
	img := p.IconURL
	if img == "" {
		img = p.SnoovatarURL
	}
	if idx := strings.IndexByte(img, '?'); idx >= 0 {
		img = img[:idx]
	}
	return img
}

// ParseProfileEvent reads a com.reddit.profile event. The content is custom, so it is decoded
// from the raw map rather than through mautrix's event types.
func ParseProfileEvent(evt *event.Event) (*Profile, bool) {
	if evt.Type.Type != ProfileEventType || evt.Content.Raw == nil {
		return nil, false
	}
	raw := evt.Content.Raw
	p := &Profile{}
	p.Username, _ = raw["username"].(string)
	p.DisplayName, _ = raw["displayname"].(string)
	p.IconURL, _ = raw["icon_url"].(string)
	p.SnoovatarURL, _ = raw["snoovatar_url"].(string)
	p.IsNSFW, _ = raw["is_nsfw"].(bool)
	if rel, ok := raw["m.relates_to"].(map[string]any); ok {
		p.RelatesToEventID, _ = rel["event_id"].(string)
	}
	if p.Name() == "" || p.AvatarURL() == "" {
		return nil, false
	}
	return p, true
}
