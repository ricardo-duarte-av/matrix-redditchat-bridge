package redditchat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultTokenRefreshURL is the Reddit chat page. Loading it with a logged-in session cookie
// hands back a freshly minted 24 hour chat token, in two places at once: a `token_v2` Set-Cookie
// header, and the `token` attribute of the page's <rs-app> element.
//
// The page also advertises a `token-refresh-url` of /svc/shreddit/token, which is what the web
// client calls at runtime. That endpoint was tried first and rejects every request shape
// attempted (403 Forbidden with a JSON body, or 400) even with valid session cookies and the
// csrf_token the other /svc/shreddit/* endpoints use, so it needs something not visible in the
// page source. Loading the page is slower but verifiably works, so it's what the bridge does.
const DefaultTokenRefreshURL = "https://www.reddit.com/chat/"

// DefaultUserAgent is sent on refresh requests. Reddit blocks obviously non-browser clients on
// the www host, so this needs to look like a browser even though the Matrix host doesn't care.
const DefaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// TokenRefresher mints fresh Reddit chat tokens from a stored Reddit web session cookie.
//
// Chat tokens are JWTs that live for 24 hours, so without this the bridge would need a manual
// re-login every day. Reddit's web client solves this by re-fetching from the refresh endpoint
// using its ordinary session cookies; this does the same thing with a cookie header the user
// supplies at login.
type TokenRefresher struct {
	// Cookie is the raw value of a Cookie header from an authenticated Reddit web session.
	Cookie string
	// URL is the refresh endpoint. Defaults to DefaultTokenRefreshURL.
	URL string
	// UserAgent defaults to DefaultUserAgent.
	UserAgent string
	// ProxyURL optionally routes refresh requests through an HTTP(S) or SOCKS5 proxy. Reddit's
	// edge blocks anonymous traffic from some networks with a "blocked by network security"
	// page; if authenticated requests are blocked too, a proxy is the way out. Only refresh
	// requests use this - the Matrix host is contacted directly.
	ProxyURL string

	HTTP *http.Client

	lock sync.Mutex
}

func (tr *TokenRefresher) httpClient() (*http.Client, error) {
	if tr.HTTP != nil {
		return tr.HTTP, nil
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if tr.ProxyURL != "" {
		proxyURL, err := url.Parse(tr.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL %q: %w", tr.ProxyURL, err)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}
	return client, nil
}

// RefreshedToken is a freshly minted chat token and the claims parsed out of it.
type RefreshedToken struct {
	Token  string
	Claims *TokenClaims
}

// ChatTokenCookie is the cookie Reddit sets on the chat page containing the chat token.
const ChatTokenCookie = "token_v2"

// rsAppTokenRegex extracts the chat token from the <rs-app token="..."> attribute, where the
// value is an HTML-escaped JSON object: {"token":"<jwt>","expires":<unix millis>}.
var rsAppTokenRegex = regexp.MustCompile(`<rs-app[^>]*\btoken="\{&quot;token&quot;:&quot;([A-Za-z0-9_.\-]+)&quot;`)

// ErrRedditBlocked means Reddit's edge refused the request outright rather than rejecting the
// credentials. It shows up for anonymous traffic from some networks and is fixed with a proxy,
// not with new credentials, so it's worth distinguishing.
var ErrRedditBlocked = errors.New("blocked by Reddit's edge (network security)")

// Refresh fetches a new chat token by loading the chat page with the stored session cookie.
//
// The token is taken from the token_v2 Set-Cookie header, falling back to the <rs-app> token
// attribute in the page body. Both carry the same value; the cookie is preferred because it
// doesn't depend on Reddit's markup staying the same.
func (tr *TokenRefresher) Refresh(ctx context.Context) (*RefreshedToken, error) {
	tr.lock.Lock()
	defer tr.lock.Unlock()

	if strings.TrimSpace(tr.Cookie) == "" {
		return nil, fmt.Errorf("no Reddit session cookie stored")
	}

	endpoint := tr.URL
	if endpoint == "" {
		endpoint = DefaultTokenRefreshURL
	}
	userAgent := tr.UserAgent
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	httpClient, err := tr.httpClient()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", tr.Cookie)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if isBlockPage(body) {
			return nil, fmt.Errorf("GET %s returned %s: %w - set refresh_proxy_url to work around it", endpoint, resp.Status, ErrRedditBlocked)
		}
		return nil, fmt.Errorf("GET %s returned %s: %s", endpoint, resp.Status, snippet(body))
	}

	token := chatTokenFromResponse(resp, body)
	if token == "" {
		if isBlockPage(body) {
			return nil, fmt.Errorf("GET %s: %w - set refresh_proxy_url to work around it", endpoint, ErrRedditBlocked)
		}
		// A logged-out session gets a perfectly valid 200 with no token in it, so this is the
		// expected result of an expired cookie rather than an unexpected failure.
		return nil, fmt.Errorf("no chat token in the response from %s - the Reddit session cookie has probably expired, log in again", endpoint)
	}
	claims, err := ParseToken(token)
	if err != nil {
		return nil, fmt.Errorf("refresh returned something that isn't a chat token: %w", err)
	}
	return &RefreshedToken{Token: token, Claims: claims}, nil
}

func chatTokenFromResponse(resp *http.Response, body []byte) string {
	for _, cookie := range resp.Cookies() {
		if cookie.Name == ChatTokenCookie && cookie.Value != "" {
			return cookie.Value
		}
	}
	if match := rsAppTokenRegex.FindSubmatch(body); match != nil {
		return string(match[1])
	}
	return ""
}

func isBlockPage(body []byte) bool {
	return bytes.Contains(body, []byte("blocked by network security")) ||
		bytes.Contains(body, []byte("whoa there, pardner"))
}

func snippet(body []byte) string {
	const max = 300
	s := strings.TrimSpace(string(body))
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

// SetToken swaps the access token used for subsequent requests.
func (c *Client) SetToken(token string) {
	c.Matrix.AccessToken = token
}
