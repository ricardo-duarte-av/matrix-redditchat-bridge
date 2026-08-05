package redditchat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// UserAboutURLTemplate is Reddit's public user info endpoint. It is the only place avatars are
// available: Reddit's Matrix profiles and member events both carry an empty avatar_url for every
// user, so there is nothing to bridge from the Matrix side.
const UserAboutURLTemplate = "https://www.reddit.com/user/%s/about.json"

// UserAbout is the subset of Reddit's user info the bridge uses.
type UserAbout struct {
	// ID is the account ID without the t2_ prefix, e.g. "3mbr7".
	ID string `json:"id"`
	// Name is the Reddit username, e.g. "daedric".
	Name string `json:"name"`
	// IconImg is the avatar headshot. Present for every account, falling back to one of
	// Reddit's default avatars.
	IconImg string `json:"icon_img"`
	// SnoovatarImg is the full-body avatar, empty for accounts that never made one.
	SnoovatarImg string `json:"snoovatar_img"`
}

// AvatarURL picks the image to use as the ghost's avatar and strips the query string, so the
// value is stable enough to use as a change-detection key.
func (ua *UserAbout) AvatarURL() string {
	img := ua.IconImg
	if img == "" {
		img = ua.SnoovatarImg
	}
	if idx := strings.IndexByte(img, '?'); idx >= 0 {
		img = img[:idx]
	}
	return img
}

// UserAbout fetches a Reddit user's public info.
//
// This goes to www.reddit.com rather than the Matrix host, so it needs the session cookie and
// browser-like headers, and it honours the same proxy setting as token refresh. Logins created
// with the token-only flow have no cookie and therefore no avatars.
func (tr *TokenRefresher) UserAbout(ctx context.Context, username string) (*UserAbout, error) {
	if strings.TrimSpace(tr.Cookie) == "" {
		return nil, fmt.Errorf("no Reddit session cookie stored")
	}
	if username == "" {
		return nil, fmt.Errorf("no username")
	}
	endpoint := fmt.Sprintf(UserAboutURLTemplate, url.PathEscape(username))
	body, err := tr.getWeb(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data UserAbout `json:"data"`
	}
	if err = json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse user info: %s", snippet(body))
	}
	if parsed.Data.Name == "" {
		return nil, fmt.Errorf("no user data in response: %s", snippet(body))
	}
	return &parsed.Data, nil
}

// DownloadWeb fetches an arbitrary reddit.com URL, used for avatar images.
func (tr *TokenRefresher) DownloadWeb(ctx context.Context, rawURL string) ([]byte, error) {
	return tr.getWeb(ctx, rawURL)
}

func (tr *TokenRefresher) getWeb(ctx context.Context, endpoint string) ([]byte, error) {
	httpClient, err := tr.httpClient()
	if err != nil {
		return nil, err
	}
	userAgent := tr.UserAgent
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", tr.Cookie)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json,image/*,*/*")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if isBlockPage(body) {
			return nil, fmt.Errorf("GET %s: %w", endpoint, ErrRedditBlocked)
		}
		return nil, fmt.Errorf("GET %s returned %s: %s", endpoint, resp.Status, snippet(body))
	}
	return body, nil
}
