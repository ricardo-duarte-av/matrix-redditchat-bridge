package connector

import (
	"encoding/json"
	"testing"

	"maunium.net/go/mautrix/event"
)

// Reddit delivers reactions nested under m.relates_to, but its own web client sends them
// flattened. Both were captured from the live service, so both must parse.
func TestRedditReactionTarget(t *testing.T) {
	cases := []struct {
		name, raw, wantTarget, wantKey string
		wantOK                         bool
	}{
		{
			name:       "as delivered by sync (nested)",
			raw:        `{"type":"m.reaction","content":{"m.relates_to":{"event_id":"$fSz8zK","key":"jvuspmbga7081.gif","rel_type":"m.annotation"}}}`,
			wantTarget: "$fSz8zK", wantKey: "jvuspmbga7081.gif", wantOK: true,
		},
		{
			name:       "as sent by Reddit's client (flattened)",
			raw:        `{"type":"m.reaction","content":{"event_id":"$fSz8zK","key":"jvuspmbga7081.gif","rel_type":"m.annotation"}}`,
			wantTarget: "$fSz8zK", wantKey: "jvuspmbga7081.gif", wantOK: true,
		},
		{name: "empty content", raw: `{"type":"m.reaction","content":{}}`, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var evt event.Event
			if err := json.Unmarshal([]byte(tc.raw), &evt); err != nil {
				t.Fatal(err)
			}
			_ = evt.Content.ParseRaw(event.EventReaction)
			target, key, ok := redditReactionTarget(&evt)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (target != tc.wantTarget || key != tc.wantKey) {
				t.Errorf("got (%q, %q), want (%q, %q)", target, key, tc.wantTarget, tc.wantKey)
			}
		})
	}
}

// Reddit emoji IDs are filenames; clients that can't render an image reaction should see
// something readable rather than "jvuspmbga7081.gif".
func TestReactionShortcode(t *testing.T) {
	for in, want := range map[string]string{
		"jvuspmbga7081.gif": ":jvuspmbga7081:",
		"foyijyyga7081.gif": ":foyijyyga7081:",
		"noext":             ":noext:",
	} {
		if got := reactionShortcode(in); got != want {
			t.Errorf("reactionShortcode(%q) = %q, want %q", in, got, want)
		}
	}
}

// The emoji key maps onto Reddit's CDN, which is where the image actually lives.
func TestRedditEmojiURL(t *testing.T) {
	if got := redditEmojiURL("jvuspmbga7081.gif"); got != "https://i.redd.it/jvuspmbga7081.gif" {
		t.Errorf("got %q", got)
	}
	for _, bad := range []string{"", "no-extension", "../etc/passwd", "has space.gif"} {
		if got := redditEmojiURL(bad); got != "" {
			t.Errorf("redditEmojiURL(%q) = %q, want empty", bad, got)
		}
	}
}
