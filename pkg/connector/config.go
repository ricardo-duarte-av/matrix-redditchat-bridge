package connector

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/util/configupgrade"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/redditchat"
)

//go:embed example-config.yaml
var ExampleConfig string

type Config struct {
	HomeserverURL string `yaml:"homeserver_url"`
	ServerName    string `yaml:"server_name"`

	SyncTimeout    time.Duration `yaml:"sync_timeout"`
	RequestTimeout time.Duration `yaml:"request_timeout"`

	BackfillBatchSize int  `yaml:"backfill_batch_size"`
	BridgeHiddenChats bool `yaml:"bridge_hidden_chats"`
	BridgeSpamChats   bool `yaml:"bridge_spam_chats"`

	PendingRequestPollInterval time.Duration `yaml:"pending_request_poll_interval"`

	MatrixMediaURL  string `yaml:"matrix_media_url"`
	TokenRefreshURL string `yaml:"token_refresh_url"`
	UserAgent       string `yaml:"user_agent"`
	RefreshProxyURL string `yaml:"refresh_proxy_url"`
}

func upgradeConfig(helper configupgrade.Helper) {
	helper.Copy(configupgrade.Str, "homeserver_url")
	helper.Copy(configupgrade.Str, "server_name")
	helper.Copy(configupgrade.Str, "sync_timeout")
	helper.Copy(configupgrade.Str, "request_timeout")
	helper.Copy(configupgrade.Int, "backfill_batch_size")
	helper.Copy(configupgrade.Bool, "bridge_hidden_chats")
	helper.Copy(configupgrade.Bool, "bridge_spam_chats")
	helper.Copy(configupgrade.Str, "pending_request_poll_interval")
	helper.Copy(configupgrade.Str|configupgrade.Null, "matrix_media_url")
	helper.Copy(configupgrade.Str, "token_refresh_url")
	helper.Copy(configupgrade.Str, "user_agent")
	helper.Copy(configupgrade.Str|configupgrade.Null, "refresh_proxy_url")
}

func (rc *RedditChatConnector) GetConfig() (string, any, configupgrade.Upgrader) {
	return ExampleConfig, &rc.Config, configupgrade.SimpleUpgrader(upgradeConfig)
}

func (rc *RedditChatConnector) ValidateConfig() error {
	if rc.Config.HomeserverURL == "" {
		return fmt.Errorf("homeserver_url is required")
	} else if !strings.HasPrefix(rc.Config.HomeserverURL, "http") {
		return fmt.Errorf("homeserver_url must be a http(s) URL")
	} else if rc.Config.ServerName == "" {
		return fmt.Errorf("server_name is required")
	}
	if rc.Config.SyncTimeout <= 0 {
		rc.Config.SyncTimeout = 30 * time.Second
	}
	if rc.Config.RequestTimeout <= 0 {
		rc.Config.RequestTimeout = 60 * time.Second
	}
	if rc.Config.BackfillBatchSize <= 0 {
		rc.Config.BackfillBatchSize = 50
	}
	if rc.Config.PendingRequestPollInterval < 0 {
		rc.Config.PendingRequestPollInterval = 0
	}
	if rc.Config.TokenRefreshURL == "" {
		rc.Config.TokenRefreshURL = redditchat.DefaultTokenRefreshURL
	}
	if rc.Config.UserAgent == "" {
		rc.Config.UserAgent = redditchat.DefaultUserAgent
	}
	return nil
}
