package redditchat

import (
	"testing"
)

// Reddit's media endpoint accepts images only, verified against the live server: text/plain,
// application/pdf, video/mp4 and application/octet-stream are all refused with
// `"<type>" is not supported format`.
func TestSupportedUploadTypes(t *testing.T) {
	for _, mime := range []string{"image/jpeg", "image/png", "image/gif", "image/webp"} {
		if !SupportedUploadTypes[mime] {
			t.Errorf("%s should be accepted", mime)
		}
	}
	for _, mime := range []string{"text/plain", "application/pdf", "video/mp4", "application/octet-stream", ""} {
		if SupportedUploadTypes[mime] {
			t.Errorf("%s is rejected by Reddit and must not be offered", mime)
		}
	}
}

// GIFs get a much larger allowance than everything else.
func TestUploadLimitFor(t *testing.T) {
	if got := UploadLimitFor("image/gif"); got != MaxGIFUploadSize {
		t.Errorf("gif limit = %d, want %d", got, MaxGIFUploadSize)
	}
	for _, mime := range []string{"image/png", "image/jpeg", "image/webp", "anything"} {
		if got := UploadLimitFor(mime); got != MaxUploadSize {
			t.Errorf("%s limit = %d, want %d", mime, got, MaxUploadSize)
		}
	}
	// These come from /_matrix/media/v3/config on the live server.
	if MaxUploadSize != 20<<20 {
		t.Errorf("MaxUploadSize = %d, want 20MB as Reddit reports", MaxUploadSize)
	}
	if MaxGIFUploadSize != 100<<20 {
		t.Errorf("MaxGIFUploadSize = %d, want 100MB as Reddit reports", MaxGIFUploadSize)
	}
}
