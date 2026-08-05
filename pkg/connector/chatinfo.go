package connector

import (
	"context"
	"errors"
	"fmt"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

func (c *RedditChatClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	return c.getChatInfo(ctx, portal)
}

// getChatInfo assembles portal metadata from the endpoints Reddit actually allows.
//
// Reddit rejects the bulk /rooms/{id}/state endpoint with a 403 even in joined rooms, so the
// member list comes from /members and name/topic/avatar are fetched as individual state events.
// Most Reddit chats are DMs with none of those set, and a missing one is a 404 rather than an
// error.
//
// In a chat the user hasn't accepted yet, /members and /state are 403 as well. That case is
// normally handled by [chatInfoFromInviteState] using what Reddit puts in sync's invite_state,
// but this function has to cope with it too, because the bridge can ask for chat info at any
// time.
func (c *RedditChatClient) getChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	roomID := id.RoomID(portal.ID)

	members, err := c.fetchMembers(ctx, roomID)
	if err != nil {
		if isNotJoined(err) {
			// Still a pending chat request: report that and leave the rest of the portal
			// metadata alone rather than overwriting it with nothing.
			pending := true
			return &bridgev2.ChatInfo{MessageRequest: &pending, CanBackfill: true}, nil
		}
		return nil, err
	}

	accepted := false
	info := &bridgev2.ChatInfo{
		Members:        members,
		CanBackfill:    true,
		MessageRequest: &accepted,
	}

	var nameContent event.RoomNameEventContent
	if found, err := c.Client.StateEvent(ctx, roomID, event.StateRoomName, "", &nameContent); err != nil {
		return nil, fmt.Errorf("failed to fetch room name: %w", err)
	} else if found && nameContent.Name != "" {
		info.Name = &nameContent.Name
	}

	var topicContent event.TopicEventContent
	if found, err := c.Client.StateEvent(ctx, roomID, event.StateTopic, "", &topicContent); err != nil {
		return nil, fmt.Errorf("failed to fetch room topic: %w", err)
	} else if found && topicContent.Topic != "" {
		info.Topic = &topicContent.Topic
	}

	var avatarContent event.RoomAvatarEventContent
	if found, err := c.Client.StateEvent(ctx, roomID, event.StateRoomAvatar, "", &avatarContent); err != nil {
		return nil, fmt.Errorf("failed to fetch room avatar: %w", err)
	} else if found && avatarContent.URL != "" {
		uri := avatarContent.URL.ParseOrIgnore()
		info.Avatar = &bridgev2.Avatar{
			ID: networkid.AvatarID(avatarContent.URL),
			Get: func(ctx context.Context) ([]byte, error) {
				return c.Client.Download(ctx, uri)
			},
		}
	}

	if members != nil && members.TotalMemberCount == 2 {
		dm := database.RoomTypeDM
		info.Type = &dm
	}
	return info, nil
}

// chatInfoFromInviteState builds portal metadata for a chat the user hasn't accepted yet.
//
// Reddit refuses every state and member endpoint for these rooms, but sync's invite_state
// carries the member events with display names, so no extra requests are needed or possible.
func (c *RedditChatClient) chatInfoFromInviteState(ctx context.Context, state []*event.Event) *bridgev2.ChatInfo {
	members := &bridgev2.ChatMemberList{
		IsFull:    true,
		MemberMap: make(bridgev2.ChatMemberMap, len(state)),
	}
	isDirect := false
	for _, evt := range state {
		if evt.Type != event.StateMember || evt.StateKey == nil {
			continue
		}
		if err := redditchat.ParseContent(evt, event.StateMember); err != nil {
			continue
		}
		member := evt.Content.AsMember()
		if member.Membership != event.MembershipJoin && member.Membership != event.MembershipInvite {
			continue
		}
		if member.IsDirect {
			isDirect = true
		}
		userID := id.UserID(*evt.StateKey)
		remoteID, ok := c.Client.ParseUserID(userID)
		if !ok {
			continue
		}
		members.MemberMap[networkid.UserID(remoteID)] = bridgev2.ChatMember{
			EventSender: c.senderOf(userID),
			Membership:  member.Membership,
			UserInfo:    c.userInfoFor(ctx, remoteID, member.Displayname),
		}
	}
	members.TotalMemberCount = len(members.MemberMap)

	pending := true
	info := &bridgev2.ChatInfo{
		Members:        members,
		CanBackfill:    true,
		MessageRequest: &pending,
	}
	if isDirect || members.TotalMemberCount == 2 {
		dm := database.RoomTypeDM
		info.Type = &dm
	}
	return info
}

// isNotJoined reports whether Reddit refused a request because the user hasn't accepted the
// chat yet. Reddit answers with a 403 ("has never joined this room") rather than a 404.
func isNotJoined(err error) bool {
	return errors.Is(err, mautrix.MForbidden)
}

func (c *RedditChatClient) fetchMembers(ctx context.Context, roomID id.RoomID) (*bridgev2.ChatMemberList, error) {
	resp, err := c.Client.Members(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Reddit room members: %w", err)
	}
	members := &bridgev2.ChatMemberList{
		IsFull:    true,
		MemberMap: make(bridgev2.ChatMemberMap, len(resp.Joined)),
	}
	for userID, member := range resp.Joined {
		remoteID, ok := c.Client.ParseUserID(userID)
		if !ok {
			continue
		}
		members.MemberMap[networkid.UserID(remoteID)] = bridgev2.ChatMember{
			EventSender: c.senderOf(userID),
			Membership:  event.MembershipJoin,
			// Attaching user info here means a chat resync also refreshes the ghost's avatar,
			// which is the only way an existing ghost ever gains one: Reddit's Matrix layer has
			// no avatars, so they never arrive with events.
			UserInfo: c.userInfoFor(ctx, remoteID, member.DisplayName),
		}
	}
	members.TotalMemberCount = len(members.MemberMap)
	return members, nil
}

func (c *RedditChatClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	userMXID := c.Client.MakeUserID(string(ghost.ID))
	profile, err := c.Client.Profile(ctx, userMXID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Reddit profile for %s: %w", ghost.ID, err)
	}
	info := &bridgev2.UserInfo{
		Identifiers: []string{fmt.Sprintf("reddit:%s", ghost.ID)},
	}
	name := profile.DisplayName
	if name != "" {
		info.Name = &name
	}
	if avatar := c.fetchAvatar(ctx, name); avatar != nil {
		info.Avatar = avatar
	}
	return info, nil
}

// fetchAvatar builds a ghost avatar from Reddit's user API.
//
// Reddit's Matrix layer has no avatars at all - every profile and member event carries an empty
// avatar_url - so the only source is reddit.com, which needs the session cookie. Logins made with
// the token-only flow have no cookie, so they simply get no avatars rather than an error.
func (c *RedditChatClient) fetchAvatar(ctx context.Context, username string) *bridgev2.Avatar {
	if username == "" || !c.CanRefresh() {
		return nil
	}
	if c.Main == nil || c.Main.aboutCache == nil {
		return nil
	}
	about := c.Main.aboutCache.get(ctx, c.web(), username)
	if about == nil {
		return nil
	}
	avatarURL := about.AvatarURL()
	if avatarURL == "" {
		return nil
	}
	mxc := c.Main.aboutCache.uploadAvatar(ctx, c.Main, c.web(), avatarURL)
	if mxc == "" {
		return nil
	}
	return &bridgev2.Avatar{
		// The URL doubles as the change key: Reddit mints a new one when the avatar changes,
		// so this avoids re-uploading the same image on every resync.
		ID:  networkid.AvatarID(avatarURL),
		MXC: mxc,
	}
}

// userInfoFor builds ghost info from a Reddit display name, which is also the Reddit username.
func (c *RedditChatClient) userInfoFor(ctx context.Context, remoteID, displayName string) *bridgev2.UserInfo {
	if displayName == "" {
		return nil
	}
	name := displayName
	info := &bridgev2.UserInfo{
		Name:        &name,
		Identifiers: []string{fmt.Sprintf("reddit:%s", remoteID)},
	}
	if avatar := c.fetchAvatar(ctx, displayName); avatar != nil {
		info.Avatar = avatar
	}
	return info
}
