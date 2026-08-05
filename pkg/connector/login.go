package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

const (
	LoginFlowIDCookie = "cookie"
	LoginFlowIDToken  = "token"

	LoginStepIDCookie = "net.daedric.redditchat.login.cookie"
	LoginStepIDToken  = "net.daedric.redditchat.login.token"
	LoginStepIDDone   = "net.daedric.redditchat.login.complete"
)

func (rc *RedditChatConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Reddit session cookie",
		Description: "Log in with a Reddit web session cookie. The bridge mints chat tokens itself and refreshes them, so this doesn't need re-doing every day.",
		ID:          LoginFlowIDCookie,
	}, {
		Name:        "Chat token",
		Description: "Log in with a single Reddit chat token. Expires after 24 hours and must then be redone manually.",
		ID:          LoginFlowIDToken,
	}}
}

func (rc *RedditChatConnector) CreateLogin(_ context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	switch flowID {
	case LoginFlowIDCookie, LoginFlowIDToken:
		return &TokenLogin{connector: rc, user: user, flowID: flowID}, nil
	default:
		return nil, fmt.Errorf("unknown login flow ID %q", flowID)
	}
}

type TokenLogin struct {
	connector *RedditChatConnector
	user      *bridgev2.User
	flowID    string
	// override is set when re-authenticating an existing login rather than adding a new one.
	override *bridgev2.UserLogin
}

var (
	_ bridgev2.LoginProcess             = (*TokenLogin)(nil)
	_ bridgev2.LoginProcessUserInput    = (*TokenLogin)(nil)
	_ bridgev2.LoginProcessWithOverride = (*TokenLogin)(nil)
)

func (tl *TokenLogin) Start(_ context.Context) (*bridgev2.LoginStep, error) {
	return tl.firstStep(), nil
}

func (tl *TokenLogin) StartWithOverride(_ context.Context, override *bridgev2.UserLogin) (*bridgev2.LoginStep, error) {
	tl.override = override
	return tl.firstStep(), nil
}

func (tl *TokenLogin) firstStep() *bridgev2.LoginStep {
	if tl.flowID == LoginFlowIDCookie {
		return tl.cookieStep()
	}
	return tl.tokenStep()
}

func (tl *TokenLogin) cookieStep() *bridgev2.LoginStep {
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepIDCookie,
		Instructions: "Paste the Cookie header from a logged-in Reddit session.\n\n" +
			"In your browser: open reddit.com while logged in, open devtools -> Network, load https://www.reddit.com/chat/, " +
			"click any request to www.reddit.com, and copy the full value of the `Cookie` request header.\n\n" +
			"The bridge uses it only to mint chat tokens, which is what lets it stay logged in past the 24 hour token lifetime.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type: bridgev2.LoginInputFieldTypeToken,
				ID:   "cookie",
				Name: "Reddit Cookie header",
				Validate: func(cookie string) (string, error) {
					cookie = strings.TrimSpace(cookie)
					if cookie == "" {
						return "", fmt.Errorf("cookie is empty")
					}
					// A logged-out session cookie header won't mint a chat token, and failing
					// here is much clearer than a confusing error from the refresh endpoint.
					if !strings.Contains(cookie, "reddit_session") && !strings.Contains(cookie, "token_v2") {
						return "", fmt.Errorf("that doesn't look like a logged-in Reddit session (no reddit_session or token_v2 cookie)")
					}
					return cookie, nil
				},
			}},
		},
	}
}

func (tl *TokenLogin) tokenStep() *bridgev2.LoginStep {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       LoginStepIDToken,
		Instructions: "Enter your Reddit chat access token. It's the access token a logged-in Reddit web session uses to talk to Reddit's Matrix server.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type: bridgev2.LoginInputFieldTypeToken,
				ID:   "token",
				Name: "Reddit chat token",
				Validate: func(token string) (string, error) {
					token = strings.TrimSpace(token)
					if token == "" {
						return "", fmt.Errorf("token is empty")
					}
					return token, nil
				},
			}},
		},
	}
}

func (tl *TokenLogin) Cancel() {}

func (tl *TokenLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	var token, cookie string
	var claims *redditchat.TokenClaims
	var err error

	if tl.flowID == LoginFlowIDCookie {
		cookie = input["cookie"]
		refreshed, refreshErr := tl.connector.newRefresher(cookie).Refresh(ctx)
		if refreshErr != nil {
			return nil, fmt.Errorf("couldn't get a chat token with that cookie: %w", refreshErr)
		}
		token, claims = refreshed.Token, refreshed.Claims
	} else {
		token = input["token"]
		// Reddit chat tokens are JWTs with a 24 hour lifetime. Checking the claims up front
		// turns the most common failure ("I pasted yesterday's token") into a precise message
		// instead of a generic rejection from Reddit.
		claims, err = redditchat.ParseToken(token)
		if err != nil {
			return nil, fmt.Errorf("that doesn't look like a Reddit chat token: %w", err)
		} else if claims.Expired() {
			return nil, fmt.Errorf("that token expired %s ago (at %s) - grab a fresh one",
				time.Since(claims.ExpiresAt).Truncate(time.Second), claims.ExpiresAt.Format(time.RFC1123))
		}
	}

	client, err := redditchat.NewClient(tl.connector.clientConfig(), "", token)
	if err != nil {
		return nil, err
	}
	redditMXID, err := client.Whoami(ctx)
	if err != nil {
		if redditchat.IsTokenError(err) {
			return nil, fmt.Errorf("that token was rejected by Reddit, check that you copied all of it and that it hasn't expired")
		}
		return nil, fmt.Errorf("failed to validate token: %w", err)
	}
	remoteID, ok := client.ParseUserID(redditMXID)
	if !ok {
		return nil, fmt.Errorf("logged in as %s, which isn't a user on %s - check server_name in the config", redditMXID, tl.connector.Config.ServerName)
	}

	loginID := networkid.UserLoginID(remoteID)
	if tl.override != nil && tl.override.ID != loginID {
		return nil, fmt.Errorf("that token is for %s, but you were re-authenticating %s", remoteID, tl.override.ID)
	}

	remoteName := remoteID
	if profile, err := client.Profile(ctx, redditMXID); err == nil && profile.DisplayName != "" {
		remoteName = profile.DisplayName
	}

	ul, err := tl.user.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: remoteName,
		RemoteProfile: status.RemoteProfile{
			Username: remoteName,
		},
		Metadata: &UserLoginMetadata{
			Token:          token,
			TokenExpiresAt: claims.ExpiresAt,
			Cookie:         cookie,
		},
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to save login: %w", err)
	}
	ul.Client.Connect(ul.Log.WithContext(ctx))

	instructions := fmt.Sprintf("Successfully logged in as %s (%s)", remoteName, remoteID)
	switch {
	case cookie != "":
		instructions += ".\n\nThe bridge will refresh chat tokens automatically using your session cookie, so you shouldn't need to log in again until that cookie expires."
	case !claims.ExpiresAt.IsZero():
		instructions += fmt.Sprintf(
			".\n\nHeads up: this token expires in %s (at %s). Reddit chat tokens only last 24 hours, so you'll need to run `login` again with a fresh one. Use the `%s` flow instead to have the bridge refresh tokens itself.",
			time.Until(claims.ExpiresAt).Truncate(time.Minute), claims.ExpiresAt.Format(time.RFC1123), LoginFlowIDCookie)
	}

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       LoginStepIDDone,
		Instructions: instructions,
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}
