package redditchat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A JWT with a valid payload but a dummy header and signature. Only the payload is ever read.
const testJWT = "e30.eyJzdWIiOiJ1c2VyIiwiZXhwIjoxNzg1OTYyMjE5LjgyOTA5NiwiaWF0IjoxNzg1ODc1ODE5LjgyOTA5NSwibGlkIjoidDJfM21icjciLCJhaWQiOiJ0Ml8zbWJyNyJ9.sig"

// chatPage reproduces the shape of Reddit's real chat page response: the token appears both as
// a token_v2 cookie and in the <rs-app> element.
func chatPage(withCookie, withAttr bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if withCookie {
			http.SetCookie(w, &http.Cookie{Name: ChatTokenCookie, Value: testJWT, Path: "/"})
		}
		http.SetCookie(w, &http.Cookie{Name: "session_tracker", Value: "abc", Path: "/"})
		w.Header().Set("Content-Type", "text/html")
		body := "<html><body>"
		if withAttr {
			body += `<rs-app class="w-full" locale="pt-PT" token="{&quot;token&quot;:&quot;` + testJWT +
				`&quot;,&quot;expires&quot;:1785962219000}" telemetry-event-app-name="web3x">`
		}
		body += "</body></html>"
		_, _ = w.Write([]byte(body))
	}
}

func TestRefreshReadsTokenFromCookie(t *testing.T) {
	var gotCookie, gotMethod, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie, gotMethod, gotUA = r.Header.Get("Cookie"), r.Method, r.Header.Get("User-Agent")
		chatPage(true, true)(w, r)
	}))
	defer srv.Close()

	tr := &TokenRefresher{Cookie: "reddit_session=abc", URL: srv.URL}
	got, err := tr.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != testJWT {
		t.Error("token mismatch")
	}
	if got.Claims.AccountID != "t2_3mbr7" {
		t.Errorf("AccountID = %q, want t2_3mbr7", got.Claims.AccountID)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotCookie != "reddit_session=abc" {
		t.Errorf("Cookie header = %q", gotCookie)
	}
	if !strings.Contains(gotUA, "Mozilla/5.0") {
		t.Errorf("User-Agent = %q, want browser-like", gotUA)
	}
}

// If Reddit stops setting the cookie, the <rs-app> attribute must still work.
func TestRefreshFallsBackToPageAttribute(t *testing.T) {
	srv := httptest.NewServer(chatPage(false, true))
	defer srv.Close()

	tr := &TokenRefresher{Cookie: "reddit_session=abc", URL: srv.URL}
	got, err := tr.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != testJWT {
		t.Errorf("token = %q, want the one from the rs-app attribute", got.Token)
	}
}

// A logged-out session returns a valid 200 page with no token; that must read as "log in again",
// not as an unexpected parse failure.
func TestRefreshDetectsLoggedOutSession(t *testing.T) {
	srv := httptest.NewServer(chatPage(false, false))
	defer srv.Close()

	tr := &TokenRefresher{Cookie: "reddit_session=stale", URL: srv.URL}
	_, err := tr.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "cookie has probably expired") {
		t.Errorf("error should point at the cookie, got: %v", err)
	}
}

// The edge block needs a proxy rather than new credentials, so it must be distinguishable.
func TestRefreshDetectsBlockPage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"403", http.StatusForbidden},
		{"200 with block body", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("<h1>whoa there, pardner!</h1><p>blocked by network security</p>"))
			}))
			defer srv.Close()

			tr := &TokenRefresher{Cookie: "reddit_session=abc", URL: srv.URL}
			_, err := tr.Refresh(context.Background())
			if !errors.Is(err, ErrRedditBlocked) {
				t.Errorf("error should be ErrRedditBlocked, got: %v", err)
			}
			if !strings.Contains(err.Error(), "refresh_proxy_url") {
				t.Errorf("error should name the workaround, got: %v", err)
			}
		})
	}
}

func TestRefreshRejectsEmptyCookie(t *testing.T) {
	tr := &TokenRefresher{Cookie: "   "}
	if _, err := tr.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error for an empty cookie")
	}
}

func TestRefreshRejectsNonToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: ChatTokenCookie, Value: "not-a-jwt", Path: "/"})
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer srv.Close()

	tr := &TokenRefresher{Cookie: "reddit_session=abc", URL: srv.URL}
	if _, err := tr.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error for a non-JWT token")
	}
}

// The proxy exists for hosts Reddit's edge blocks outright, so verify the request goes through it.
func TestRefreshUsesProxy(t *testing.T) {
	var proxiedHost string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxiedHost = r.Host
		chatPage(true, false)(w, r)
	}))
	defer proxy.Close()

	tr := &TokenRefresher{
		Cookie:   "reddit_session=abc",
		URL:      "http://www.reddit.com/chat/",
		ProxyURL: proxy.URL,
	}
	if _, err := tr.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if proxiedHost != "www.reddit.com" {
		t.Errorf("proxy saw host %q, want www.reddit.com", proxiedHost)
	}
}

func TestRefreshRejectsBadProxyURL(t *testing.T) {
	tr := &TokenRefresher{Cookie: "reddit_session=abc", ProxyURL: "://not a url"}
	_, err := tr.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("expected a proxy URL error, got: %v", err)
	}
}

func TestSetTokenSwapsAccessToken(t *testing.T) {
	client, err := NewClient(Config{
		HomeserverURL:  "https://matrix.redditspace.com",
		ServerName:     "reddit.com",
		RequestTimeout: time.Second,
	}, "@t2_3mbr7:reddit.com", "old")
	if err != nil {
		t.Fatal(err)
	}
	client.SetToken("new")
	if client.Matrix.AccessToken != "new" {
		t.Errorf("AccessToken = %q, want new", client.Matrix.AccessToken)
	}
}
