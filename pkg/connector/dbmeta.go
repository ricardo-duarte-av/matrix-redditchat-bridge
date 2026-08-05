package connector

import (
	"time"

	"maunium.net/go/mautrix/bridgev2/database"
)

// UserLoginMetadata is stored in the user_login table for each bridged Reddit account.
type UserLoginMetadata struct {
	// The Reddit chat access token provided by the user at login.
	Token string `json:"token"`
	// When the token expires, read from its JWT claims. Reddit issues chat tokens with a 24
	// hour lifetime, so this is stored to warn the user before the bridge goes dark.
	TokenExpiresAt time.Time `json:"token_expires_at,omitzero"`
	// Cookie is the raw Cookie header from an authenticated Reddit web session. When present,
	// the bridge mints fresh chat tokens itself instead of needing a daily manual re-login.
	Cookie string `json:"cookie,omitempty"`
	// The /sync token to resume from, so restarts don't replay the whole timeline.
	NextBatch string `json:"next_batch,omitempty"`
}

func (rc *RedditChatConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		UserLogin: func() any {
			return &UserLoginMetadata{}
		},
	}
}
