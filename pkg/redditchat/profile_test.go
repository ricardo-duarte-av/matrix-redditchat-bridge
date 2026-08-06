package redditchat

import (
	"encoding/json"
	"testing"

	"maunium.net/go/mautrix/event"
)

// Captured from the live server. com.reddit.profile is the only place avatars exist inside
// Reddit's Matrix layer, and it names no user - attribution goes through m.relates_to.
const realProfileEvent = `{
  "type": "com.reddit.profile",
  "sender": "@t2_1qwk:reddit.com",
  "content": {
    "username": "ExampleUser",
    "icon_url": "https://styles.redditmedia.com/t5_3p0mey/styles/profileIcon_snoo.png?width=96&s=04bd333",
    "snoovatar_url": "https://i.redd.it/snoovatar/avatars/1141fac4.png",
    "m.relates_to": {"event_id": "$TO96v7Lw", "rel_type": "com.reddit.profile"}
  }
}`

func TestParseProfileEvent(t *testing.T) {
	var evt event.Event
	if err := json.Unmarshal([]byte(realProfileEvent), &evt); err != nil {
		t.Fatal(err)
	}
	p, ok := ParseProfileEvent(&evt)
	if !ok {
		t.Fatal("a real profile event should parse")
	}
	if p.Name() != "ExampleUser" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.RelatesToEventID != "$TO96v7Lw" {
		t.Errorf("RelatesToEventID = %q", p.RelatesToEventID)
	}
	// The signed query changes on every fetch, so it must not be part of the change key.
	want := "https://styles.redditmedia.com/t5_3p0mey/styles/profileIcon_snoo.png"
	if got := p.AvatarURL(); got != want {
		t.Errorf("AvatarURL = %q, want %q (query stripped)", got, want)
	}
}

func TestParseProfileEventRejectsOthers(t *testing.T) {
	for name, raw := range map[string]string{
		"wrong type": `{"type":"m.room.message","content":{"username":"x","icon_url":"http://a/b.png"}}`,
		"no avatar":  `{"type":"com.reddit.profile","content":{"username":"x"}}`,
		"no name":    `{"type":"com.reddit.profile","content":{"icon_url":"http://a/b.png"}}`,
		"no content": `{"type":"com.reddit.profile"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var evt event.Event
			if err := json.Unmarshal([]byte(raw), &evt); err != nil {
				t.Fatal(err)
			}
			if _, ok := ParseProfileEvent(&evt); ok {
				t.Error("should not have parsed")
			}
		})
	}
}

// Accounts with no snoovatar still have an icon_url; accounts with both prefer the icon.
func TestProfileAvatarFallback(t *testing.T) {
	iconOnly := &Profile{Username: "a", IconURL: "https://i/x.png"}
	if got := iconOnly.AvatarURL(); got != "https://i/x.png" {
		t.Errorf("got %q", got)
	}
	snooOnly := &Profile{Username: "a", SnoovatarURL: "https://i/s.png"}
	if got := snooOnly.AvatarURL(); got != "https://i/s.png" {
		t.Errorf("got %q", got)
	}
}
