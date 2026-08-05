package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

// refreshMargin is how long before expiry the bridge proactively mints a new token, so a
// refresh never races an in-flight request.
const refreshMargin = 1 * time.Hour

func (rc *RedditChatConnector) newRefresher(cookie string) *redditchat.TokenRefresher {
	return &redditchat.TokenRefresher{
		Cookie:    cookie,
		URL:       rc.Config.TokenRefreshURL,
		UserAgent: rc.Config.UserAgent,
		ProxyURL:  rc.Config.RefreshProxyURL,
	}
}

// web returns the reddit.com client, rebuilding it if the cookie changed since login.
func (c *RedditChatClient) web() *redditchat.TokenRefresher {
	if c.Web == nil || c.Web.Cookie != c.meta().Cookie {
		c.Web = c.Main.newRefresher(c.meta().Cookie)
	}
	return c.Web
}

// CanRefresh reports whether this login stored a session cookie, i.e. whether the bridge can
// keep itself logged in without the user pasting a new token every day.
func (c *RedditChatClient) CanRefresh() bool {
	return c.meta().Cookie != ""
}

// ensureFreshToken refreshes the chat token if it is expired or close to it. It returns whether
// the token was replaced. Errors are returned unwrapped so callers can decide whether a failed
// refresh is fatal.
func (c *RedditChatClient) ensureFreshToken(ctx context.Context, force bool) (bool, error) {
	meta := c.meta()
	if meta.Cookie == "" {
		return false, nil
	}
	if !force && !meta.TokenExpiresAt.IsZero() && time.Until(meta.TokenExpiresAt) > refreshMargin {
		return false, nil
	}

	c.refreshLock.Lock()
	defer c.refreshLock.Unlock()
	// Re-check under the lock: a concurrent caller may have already refreshed.
	if !force && !meta.TokenExpiresAt.IsZero() && time.Until(meta.TokenExpiresAt) > refreshMargin {
		return false, nil
	}

	log := zerolog.Ctx(ctx)
	log.Debug().Time("old_expiry", meta.TokenExpiresAt).Msg("Refreshing Reddit chat token")

	refreshed, err := c.web().Refresh(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to refresh Reddit chat token: %w", err)
	}

	meta.Token = refreshed.Token
	meta.TokenExpiresAt = refreshed.Claims.ExpiresAt
	c.Client.SetToken(refreshed.Token)
	if err = c.UserLogin.Save(ctx); err != nil {
		return true, fmt.Errorf("refreshed token but failed to save it: %w", err)
	}
	log.Info().Time("expires_at", refreshed.Claims.ExpiresAt).Msg("Refreshed Reddit chat token")
	return true, nil
}
