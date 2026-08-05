// Package redditchat wraps a mautrix.Client pointed at Reddit's modified, unfederated
// Dendrite instance. Reddit chat speaks enough of the Matrix client-server API that a normal
// client can use it, so the bridge's "network client" is just another Matrix client. Every
// Reddit-specific quirk should be contained in this package so the connector can stay generic.
package redditchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// UserIDRegex matches the localpart of a Reddit user ID, e.g. t2_abcdef in @t2_abcdef:reddit.com.
var UserIDRegex = regexp.MustCompile(`^t2_[a-z0-9]+$`)

// ParseContent parses an event's content if it isn't parsed already.
//
// [event.Content.ParseRaw] reports "content is already parsed" as an error, but mautrix parses
// content eagerly in some code paths and not others, so that condition is a success here.
// Treating it as a failure silently discards every already-parsed event.
func ParseContent(evt *event.Event, evtType event.Type) error {
	err := evt.Content.ParseRaw(evtType)
	if err != nil && !errors.Is(err, event.ErrContentAlreadyParsed) {
		return err
	}
	return nil
}

type Config struct {
	HomeserverURL  string
	ServerName     string
	RequestTimeout time.Duration
}

type Client struct {
	Matrix *mautrix.Client
	Config Config

	// UserID is the full Reddit MXID of the logged-in account, e.g. @t2_abcdef:reddit.com.
	UserID id.UserID
}

// NewClient creates a Reddit chat client using a Reddit chat access token. The user ID is
// optional: Reddit's /whoami tells us who we are, so [Client.Whoami] should be called before
// the client is used for anything else.
func NewClient(cfg Config, userID id.UserID, token string) (*Client, error) {
	mxClient, err := mautrix.NewClient(cfg.HomeserverURL, userID, token)
	if err != nil {
		return nil, fmt.Errorf("failed to create Matrix client for Reddit: %w", err)
	}
	// Sync uses its own client with a longer timeout, see Sync below.
	mxClient.Client = &http.Client{Timeout: cfg.RequestTimeout}
	mxClient.DefaultHTTPRetries = 3
	return &Client{Matrix: mxClient, Config: cfg, UserID: userID}, nil
}

// Whoami validates the access token and fills in the client's own user ID.
func (c *Client) Whoami(ctx context.Context) (id.UserID, error) {
	resp, err := c.Matrix.Whoami(ctx)
	if err != nil {
		return "", err
	}
	c.UserID = resp.UserID
	c.Matrix.UserID = resp.UserID
	return resp.UserID, nil
}

// ParseUserID converts a full Reddit MXID into the bare Reddit user ID used as the bridge's
// network user ID (e.g. @t2_abcdef:reddit.com -> t2_abcdef).
func (c *Client) ParseUserID(mxid id.UserID) (string, bool) {
	localpart, server, err := mxid.Parse()
	if err != nil || server != c.Config.ServerName {
		return "", false
	}
	return localpart, true
}

// MakeUserID is the inverse of ParseUserID.
func (c *Client) MakeUserID(userID string) id.UserID {
	return id.NewUserID(userID, c.Config.ServerName)
}

// Reddit's own client excludes these custom event types from both sync and pagination. They're
// internal moderation signals with no Matrix equivalent.
var redditIgnoredEventTypes = []event.Type{
	{Type: "com.reddit.review_open", Class: event.MessageEventType},
	{Type: "com.reddit.review_close", Class: event.MessageEventType},
}

// roomEventFilter mirrors the filter Reddit's own web client sends. Full member lists are
// fetched separately via /members, so lazy loading here is safe and keeps sync responses small.
func roomEventFilter() *mautrix.FilterPart {
	return &mautrix.FilterPart{
		NotTypes:                  redditIgnoredEventTypes,
		LazyLoadMembers:           true,
		UnreadThreadNotifications: true,
	}
}

// syncFilter is passed inline as JSON rather than pre-registered with /user/{id}/filter, which
// is what Reddit's client does and avoids depending on their filter storage working.
func syncFilter() (string, error) {
	filter, err := json.Marshal(&mautrix.Filter{
		Room: &mautrix.RoomFilter{
			Timeline: roomEventFilter(),
			State:    &mautrix.FilterPart{LazyLoadMembers: true},
		},
	})
	if err != nil {
		return "", err
	}
	return string(filter), nil
}

// Sync performs a single /sync request. The caller owns the since token and is responsible for
// persisting it. A dedicated http.Client is used because the sync request is held open for
// much longer than the normal request timeout allows.
func (c *Client) Sync(ctx context.Context, since string, timeout time.Duration) (*mautrix.RespSync, error) {
	filter, err := syncFilter()
	if err != nil {
		return nil, err
	}
	return c.Matrix.FullSyncRequest(ctx, mautrix.ReqSync{
		Timeout: int(timeout.Milliseconds()),
		Since:   since,
		// The `filter` query parameter accepts either a filter ID or inline JSON; mautrix puts
		// whatever is in FilterID straight into the query, so inline JSON goes here.
		FilterID:    filter,
		FullState:   false,
		SetPresence: event.PresenceOffline,
		Client: &http.Client{
			Timeout: timeout + c.Config.RequestTimeout,
		},
	})
}

// SendText sends a plain text message and returns the Reddit event ID.
func (c *Client) SendText(ctx context.Context, roomID id.RoomID, content *event.MessageEventContent, txnID string) (id.EventID, error) {
	resp, err := c.Matrix.SendMessageEvent(ctx, roomID, event.EventMessage, content, mautrix.ReqSendEvent{
		TransactionID: txnID,
	})
	if err != nil {
		return "", err
	}
	return resp.EventID, nil
}

// EndOfTimelineToken is the pagination token Reddit returns once a room's history has been
// walked back to its creation. Paginating backwards from it returns an empty chunk forever, so
// it must be treated as a terminator rather than as a usable cursor.
//
// Reddit's own client sends `from=t0_0&dir=b`, which looks like a starting point but is really
// this same "already at the beginning" case. Backward pagination starts with `from` omitted.
const EndOfTimelineToken = "t0_0"

// Messages paginates a room's timeline. dir is mautrix.DirectionBackward or DirectionForward.
// An empty `from` starts from the most recent message.
func (c *Client) Messages(ctx context.Context, roomID id.RoomID, from string, dir mautrix.Direction, limit int) (*mautrix.RespMessages, error) {
	return c.Matrix.Messages(ctx, roomID, from, "", dir, roomEventFilter(), limit)
}

// HasMoreHistory reports whether a pagination response leaves any history left to fetch.
func HasMoreHistory(resp *mautrix.RespMessages) bool {
	return resp.End != "" && resp.End != EndOfTimelineToken && len(resp.Chunk) > 0
}

// Members returns the room's member events.
//
// Reddit rejects the full /state endpoint with a 403, so the member list has to come from
// /members and individual state events have to be fetched one at a time.
func (c *Client) Members(ctx context.Context, roomID id.RoomID) (*mautrix.RespJoinedMembers, error) {
	resp, err := c.Matrix.Members(ctx, roomID)
	if err != nil {
		return nil, err
	}
	joined := &mautrix.RespJoinedMembers{Joined: make(map[id.UserID]mautrix.JoinedMember, len(resp.Chunk))}
	for _, evt := range resp.Chunk {
		if evt.StateKey == nil {
			continue
		}
		if err = ParseContent(evt, event.StateMember); err != nil {
			continue
		}
		member := evt.Content.AsMember()
		if member.Membership != event.MembershipJoin && member.Membership != event.MembershipInvite {
			continue
		}
		joined.Joined[id.UserID(*evt.StateKey)] = mautrix.JoinedMember{
			DisplayName: member.Displayname,
			AvatarURL:   string(member.AvatarURL),
		}
	}
	return joined, nil
}

// StateEvent fetches a single room state event, reporting found=false for a missing one rather
// than an error. Reddit chat rooms legitimately have no m.room.name, topic or avatar.
func (c *Client) StateEvent(ctx context.Context, roomID id.RoomID, evtType event.Type, stateKey string, into any) (found bool, err error) {
	err = c.Matrix.StateEvent(ctx, roomID, evtType, stateKey, into)
	if err != nil {
		if errors.Is(err, mautrix.MNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *Client) Profile(ctx context.Context, userID id.UserID) (*mautrix.RespUserProfile, error) {
	return c.Matrix.GetProfile(ctx, userID)
}

func (c *Client) JoinRoom(ctx context.Context, roomID id.RoomID) error {
	_, err := c.Matrix.JoinRoomByID(ctx, roomID)
	return err
}

// AvatarMXC downloads a Reddit mxc:// URI so it can be reuploaded to the Matrix side.
func (c *Client) Download(ctx context.Context, uri id.ContentURI) ([]byte, error) {
	return c.Matrix.DownloadBytes(ctx, uri)
}

// IsTokenError reports whether an error means the Reddit chat token is no longer usable, which
// is the one failure mode that needs the user to run login again rather than just a retry.
func IsTokenError(err error) bool {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.RespError == nil {
		return httpErr.Response != nil && httpErr.Response.StatusCode == http.StatusUnauthorized
	}
	switch httpErr.RespError.ErrCode {
	case "M_UNKNOWN_TOKEN", "M_MISSING_TOKEN":
		return true
	}
	return strings.Contains(strings.ToUpper(httpErr.RespError.ErrCode), "TOKEN")
}

const (
	// HiddenChatAccountData marks a chat the user dismissed. Reddit's "Ignore" button on a chat
	// request sets {"hidden": true} here, and it is the ONLY trace the action leaves: the room,
	// its membership and its timeline are all byte-identical before and after. Reddit's UI then
	// stops showing the chat anywhere, so bridging a hidden chat would resurrect a conversation
	// the user deliberately got rid of.
	HiddenChatAccountData = "com.reddit.hidden_chat"

	// InviteSpamStatusEvent carries Reddit's spam classification of an unaccepted chat. Reddit
	// writes "unverified" when the room is created and updates it a second or two later.
	// Chats classified as spam are not shown in Reddit's requests list.
	InviteSpamStatusEvent = "com.reddit.invite_spam_status"

	// SpamStatusSpam is the value Reddit uses for a chat request it classified as spam.
	SpamStatusSpam = "spam"
)

type hiddenChatContent struct {
	Hidden bool `json:"hidden"`
}

type spamStatusContent struct {
	Status string `json:"status"`
}

// IsHiddenChat reports whether the user dismissed this chat on Reddit.
//
// Room account data is only delivered in /sync for joined rooms, so unaccepted chats have to be
// queried directly. A missing key means "not hidden".
func (c *Client) IsHiddenChat(ctx context.Context, roomID id.RoomID) (bool, error) {
	var content hiddenChatContent
	err := c.Matrix.GetRoomAccountData(ctx, roomID, HiddenChatAccountData, &content)
	if err != nil {
		if errors.Is(err, mautrix.MNotFound) {
			return false, nil
		}
		return false, err
	}
	return content.Hidden, nil
}

// IsHiddenChatInSync reads the same flag out of a joined room's sync account data, which avoids
// an extra request for rooms the user has already accepted.
func IsHiddenChatInSync(events []*event.Event) bool {
	for _, evt := range events {
		if evt.Type.Type != HiddenChatAccountData {
			continue
		}
		var content hiddenChatContent
		if raw, err := json.Marshal(evt.Content.Raw); err == nil {
			_ = json.Unmarshal(raw, &content)
		}
		return content.Hidden
	}
	return false
}

// IsSpamChat reports whether Reddit classified an unaccepted chat as spam.
//
// The status lives in the room timeline rather than in state, and Reddit writes it more than
// once, so the newest value wins. Pagination returns newest-first, which is what makes taking
// the first match correct.
func (c *Client) IsSpamChat(ctx context.Context, roomID id.RoomID) (bool, error) {
	resp, err := c.Matrix.Messages(ctx, roomID, "", "", mautrix.DirectionBackward, nil, 30)
	if err != nil {
		return false, err
	}
	for _, evt := range resp.Chunk {
		if evt.Type.Type != InviteSpamStatusEvent {
			continue
		}
		var content spamStatusContent
		if raw, err := json.Marshal(evt.Content.Raw); err == nil {
			_ = json.Unmarshal(raw, &content)
		}
		return content.Status == SpamStatusSpam, nil
	}
	return false, nil
}

// JoinedRooms lists every room the account has accepted.
//
// Reddit's /sync caps its room list at 20 joined and 20 invited rooms regardless of filter,
// timeline limit or full_state, so an account with more chats than that would never see the rest
// through sync alone. This endpoint is not capped and is the only way to enumerate them all.
func (c *Client) JoinedRooms(ctx context.Context) ([]id.RoomID, error) {
	resp, err := c.Matrix.JoinedRooms(ctx)
	if err != nil {
		return nil, err
	}
	return resp.JoinedRooms, nil
}
