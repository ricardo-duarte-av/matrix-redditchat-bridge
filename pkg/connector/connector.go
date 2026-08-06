package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
)

type RedditChatConnector struct {
	br     *bridgev2.Bridge
	Config Config

	// aboutCache keeps reddit.com lookups and avatar uploads from being repeated.
	aboutCache *aboutCache
	// profiles holds avatars learned from com.reddit.profile events, which is the in-band
	// source and needs no session cookie.
	profiles *profileStore
}

var (
	_ bridgev2.NetworkConnector            = (*RedditChatConnector)(nil)
	_ bridgev2.ConfigValidatingNetwork     = (*RedditChatConnector)(nil)
	_ bridgev2.IdentifierValidatingNetwork = (*RedditChatConnector)(nil)
)

func NewConnector() *RedditChatConnector {
	return &RedditChatConnector{aboutCache: newAboutCache(), profiles: newProfileStore()}
}

func (rc *RedditChatConnector) Init(bridge *bridgev2.Bridge) {
	rc.br = bridge
}

func (rc *RedditChatConnector) Start(_ context.Context) error {
	return nil
}

func (rc *RedditChatConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "Reddit Chat",
		NetworkURL:           "https://www.reddit.com/chat",
		NetworkIcon:          "",
		NetworkID:            "redditchat",
		BeeperBridgeType:     "github.com/ricardo-duarte-av/matrix-redditchat-bridge",
		DefaultPort:          29340,
		DefaultCommandPrefix: "!rc",
	}
}

func (rc *RedditChatConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		// Reddit's Matrix layer carries no avatars, so ghost info has to be re-requested from
		// reddit.com rather than arriving with events. Without this, a ghost that already has a
		// name would never gain an avatar. The about cache keeps the cost down.
		AggressiveUpdateInfo: true,
	}
}

func (rc *RedditChatConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 2
}
