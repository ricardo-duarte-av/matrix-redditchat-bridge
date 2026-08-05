package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/id"
)

// uploadViaMediaURL uploads media straight to a configured Matrix media endpoint, bypassing the
// homeserver address the rest of the bridge uses.
//
// This exists for worker deployments where the public homeserver URL doesn't reach the media
// repository - for instance an nginx upstream pointing at a process that doesn't implement media,
// which answers M_UNRECOGNIZED for every media request. Uploading through the normal path is
// still the default; this only runs when network.matrix_media_url is set.
func (rc *RedditChatConnector) uploadViaMediaURL(ctx context.Context, data []byte, fileName, mimeType string) (id.ContentURIString, error) {
	base := strings.TrimSuffix(rc.Config.MatrixMediaURL, "/")
	if base == "" {
		return "", fmt.Errorf("no matrix_media_url configured")
	}
	token, err := rc.appserviceToken()
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("%s/_matrix/media/v3/upload?filename=%s", base, url.QueryEscape(fileName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mimeType)

	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("upload to %s failed: %w", base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload to %s returned %s: %s", base, resp.Status, truncate(body))
	}
	var parsed struct {
		ContentURI id.ContentURIString `json:"content_uri"`
	}
	if err = json.Unmarshal(body, &parsed); err != nil || parsed.ContentURI == "" {
		return "", fmt.Errorf("upload response had no content_uri: %s", truncate(body))
	}
	return parsed.ContentURI, nil
}

// appserviceToken digs the as_token out of the Matrix connector, since uploading outside the
// normal client means authenticating the request here.
func (rc *RedditChatConnector) appserviceToken() (string, error) {
	conn, ok := rc.br.Matrix.(*matrix.Connector)
	if !ok || conn.AS == nil || conn.AS.Registration == nil {
		return "", fmt.Errorf("could not read the appservice token")
	}
	return conn.AS.Registration.AppToken, nil
}

func truncate(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
