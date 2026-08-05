package connector

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

type RedditChatClient struct {
	Main      *RedditChatConnector
	UserLogin *bridgev2.UserLogin
	Client    *redditchat.Client
	Web       *redditchat.TokenRefresher

	// syncLock guards the sync goroutine's lifecycle.
	syncLock   sync.Mutex
	syncCancel context.CancelFunc
	syncDone   chan struct{}
	pollDone   chan struct{}

	// refreshLock serialises token refreshes so a 401 retry and the proactive refresh don't
	// both mint a token.
	refreshLock sync.Mutex

	loggedIn atomic.Bool

	// noticedPortals remembers where the unsupported-reaction notice was already posted, so a
	// user reacting repeatedly gets told once rather than every time.
	noticeLock     sync.Mutex
	noticedPortals map[id.RoomID]bool
}

var (
	_ bridgev2.NetworkAPI                        = (*RedditChatClient)(nil)
	_ bridgev2.BackfillingNetworkAPI             = (*RedditChatClient)(nil)
	_ bridgev2.MessageRequestAcceptingNetworkAPI = (*RedditChatClient)(nil)
)

func (rc *RedditChatConnector) clientConfig() redditchat.Config {
	return redditchat.Config{
		HomeserverURL:  rc.Config.HomeserverURL,
		ServerName:     rc.Config.ServerName,
		RequestTimeout: rc.Config.RequestTimeout,
	}
}

func (rc *RedditChatConnector) ValidateUserID(id networkid.UserID) bool {
	return redditchat.UserIDRegex.MatchString(string(id))
}

func (rc *RedditChatConnector) LoadUserLogin(_ context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)
	if meta.Token == "" {
		return fmt.Errorf("no Reddit chat token stored for %s", login.ID)
	}
	client, err := redditchat.NewClient(rc.clientConfig(), redditMXIDOf(rc, login.ID), meta.Token)
	if err != nil {
		return err
	}
	login.Client = &RedditChatClient{
		Main:      rc,
		UserLogin: login,
		Client:    client,
		// Avatars and token refresh both go to reddit.com rather than the Matrix host, and both
		// need the session cookie. Nil when the user logged in with a bare token.
		Web: rc.newRefresher(meta.Cookie),
	}
	return nil
}

func redditMXIDOf(rc *RedditChatConnector, loginID networkid.UserLoginID) id.UserID {
	return id.NewUserID(string(loginID), rc.Config.ServerName)
}

func (c *RedditChatClient) meta() *UserLoginMetadata {
	return c.UserLogin.Metadata.(*UserLoginMetadata)
}

// portalKey builds the portal key for a Reddit room. The receiver is always set: Reddit room IDs
// are globally unique, but keeping portals per-login avoids any ambiguity about which account's
// token relays a message when two bridged accounts share a Reddit room.
func (c *RedditChatClient) portalKey(roomID id.RoomID) networkid.PortalKey {
	return networkid.PortalKey{
		ID:       networkid.PortalID(roomID),
		Receiver: c.UserLogin.ID,
	}
}

func (c *RedditChatClient) senderOf(userID id.UserID) bridgev2.EventSender {
	remoteID, ok := c.Client.ParseUserID(userID)
	if !ok {
		return bridgev2.EventSender{Sender: networkid.UserID(userID)}
	}
	isMe := networkid.UserLoginID(remoteID) == c.UserLogin.ID
	sender := bridgev2.EventSender{
		IsFromMe: isMe,
		Sender:   networkid.UserID(remoteID),
	}
	if isMe {
		sender.SenderLogin = c.UserLogin.ID
	}
	return sender
}

func (c *RedditChatClient) Connect(ctx context.Context) {
	c.syncLock.Lock()
	defer c.syncLock.Unlock()
	if c.syncCancel != nil {
		return
	}
	if _, err := c.ensureFreshToken(ctx, false); err != nil {
		c.UserLogin.Log.Err(err).Msg("Failed to refresh Reddit chat token on connect")
	}
	if expiry := c.meta().TokenExpiresAt; !expiry.IsZero() && time.Now().After(expiry) {
		c.loggedIn.Store(false)
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "redditchat-token-expired",
			Message:    c.expiredMessage(expiry),
		})
		return
	}
	if _, err := c.Client.Whoami(ctx); err != nil {
		c.loggedIn.Store(false)
		c.reportError(err, "failed to connect to Reddit chat")
		return
	}
	c.loggedIn.Store(true)
	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})

	syncCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.syncCancel = cancel
	c.syncDone = make(chan struct{})
	c.pollDone = make(chan struct{})
	go c.syncLoop(syncCtx, c.syncDone)
	go c.pollPendingRequests(syncCtx, c.pollDone)
}

func (c *RedditChatClient) Disconnect() {
	c.syncLock.Lock()
	cancel, done, pollDone := c.syncCancel, c.syncDone, c.pollDone
	c.syncCancel, c.syncDone, c.pollDone = nil, nil, nil
	c.syncLock.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	if pollDone != nil {
		<-pollDone
	}
}

func (c *RedditChatClient) IsLoggedIn() bool {
	return c.loggedIn.Load()
}

// LogoutRemote only drops local state. Reddit chat tokens are the user's real session token,
// not something the bridge minted, so invalidating it would log them out of Reddit everywhere.
func (c *RedditChatClient) LogoutRemote(_ context.Context) {
	c.Disconnect()
	c.loggedIn.Store(false)
}

func (c *RedditChatClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return string(userID) == string(c.UserLogin.ID)
}

func (c *RedditChatClient) GetCapabilities(_ context.Context, _ *bridgev2.Portal) *event.RoomFeatures {
	// v1 bridges plain text only. Everything else is explicitly rejected so Matrix clients
	// don't offer users features that would silently do nothing.
	return &event.RoomFeatures{
		ID: "net.daedric.redditchat.capabilities.v5",
		// Images work in both directions. Reddit accepts nothing else - text, PDF, video and
		// octet-stream are all refused with `"<type>" is not supported format` - so other file
		// types are rejected here rather than failing after the user has sent them.
		File: event.FileFeatureMap{
			event.MsgImage: {
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"image/jpeg": event.CapLevelFullySupported,
					"image/png":  event.CapLevelFullySupported,
					"image/gif":  event.CapLevelFullySupported,
					"image/webp": event.CapLevelFullySupported,
				},
				// Reddit's image events carry no caption field.
				Caption: event.CapLevelDropped,
				MaxSize: redditchat.MaxUploadSize,
			},
			event.MsgVideo: {MimeTypes: map[string]event.CapabilitySupportLevel{"*": event.CapLevelRejected}},
			event.MsgAudio: {MimeTypes: map[string]event.CapabilitySupportLevel{"*": event.CapLevelRejected}},
			event.MsgFile:  {MimeTypes: map[string]event.CapabilitySupportLevel{"*": event.CapLevelRejected}},
		},
		// Reddit models replies as threads, so a Matrix reply is converted rather than sent
		// as-is - which is what PartialSupport means.
		Reply:  event.CapLevelPartialSupport,
		Thread: event.CapLevelFullySupported,
		// Reactions arrive from Reddit and are bridged, but cannot be sent: Reddit only accepts
		// keys from its own fixed emoji set and refuses unicode with M_INVALID_ARGUMENT_VALUE
		// "reaction key is not supported". The bridge posts a notice explaining that when a
		// Matrix user tries.
		Reaction:             event.CapLevelRejected,
		CustomEmojiReactions: true,
		Edit:                 event.CapLevelRejected,
		Delete:               event.CapLevelRejected,
		LocationMessage:      event.CapLevelRejected,
		Poll:                 event.CapLevelRejected,
		// Replying accepts a Reddit chat request, but not implicitly: the bridge has to join the
		// room first or Reddit rejects the send with "room auth reject due to event auth
		// rejected". CapLevelFullySupported here would tell the central module the network
		// handles that itself and it would skip calling HandleMatrixAcceptMessageRequest, so
		// this must stay at PartialSupport for the join to happen.
		MessageRequest: &event.MessageRequestFeatures{
			AcceptWithMessage: event.CapLevelPartialSupport,
			AcceptWithButton:  event.CapLevelFullySupported,
		},
	}
}

// expiredMessage explains what to do about a dead token. The advice differs depending on
// whether the login stored a session cookie, since only then can the bridge fix it itself.
func (c *RedditChatClient) expiredMessage(expiry time.Time) string {
	if c.CanRefresh() {
		return fmt.Sprintf(
			"Your Reddit chat token expired at %s and could not be refreshed. Your Reddit session cookie has probably expired too - run `login` again.",
			expiry.Format(time.RFC1123))
	}
	return fmt.Sprintf(
		"Your Reddit chat token expired at %s. Run `login` again with a fresh token, or use the cookie login flow so the bridge can refresh tokens itself.",
		expiry.Format(time.RFC1123))
}

// reportError maps a Reddit error onto a bridge state so the user sees something actionable in
// the management room instead of only a log line.
func (c *RedditChatClient) reportError(err error, msg string) {
	state := status.BridgeState{
		StateEvent: status.StateTransientDisconnect,
		Error:      "redditchat-sync-error",
		Message:    fmt.Sprintf("%s: %v", msg, err),
	}
	if redditchat.IsTokenError(err) {
		c.loggedIn.Store(false)
		state.StateEvent = status.StateBadCredentials
		state.Error = "redditchat-bad-token"
		state.Message = "Your Reddit chat token is no longer valid. Run `login` again with a fresh token."
	}
	c.UserLogin.BridgeState.Send(state)
}
