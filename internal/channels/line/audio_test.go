package line

import "testing"

func TestExtensionForAudio(t *testing.T) {
	cases := []struct {
		contentType string
		wantExt     string
		wantKnown   bool
	}{
		// LINE iOS voice messages — confirmed live in production 2026-04-08.
		{"audio/x-m4a", ".m4a", true},
		// Other m4a / mp4 audio variants.
		{"audio/m4a", ".m4a", true},
		{"audio/mp4", ".m4a", true},
		// AAC variants.
		{"audio/aac", ".aac", true},
		{"audio/x-aac", ".aac", true},
		// MP3 variants.
		{"audio/mp3", ".mp3", true},
		{"audio/mpeg", ".mp3", true},
		// WAV variants.
		{"audio/wav", ".wav", true},
		{"audio/wave", ".wav", true},
		{"audio/x-wav", ".wav", true},
		// Ogg / Opus.
		{"audio/ogg", ".ogg", true},
		{"audio/opus", ".opus", true},
		// Unknown types fall back to .bin and report not-known so the caller
		// can log a warning.
		{"audio/some-future-codec", ".bin", false},
		{"image/png", ".bin", false},
		{"", ".bin", false},
	}

	for _, tc := range cases {
		t.Run(tc.contentType, func(t *testing.T) {
			gotExt, gotKnown := extensionForAudio(tc.contentType)
			if gotExt != tc.wantExt {
				t.Errorf("ext: got %q, want %q", gotExt, tc.wantExt)
			}
			if gotKnown != tc.wantKnown {
				t.Errorf("known: got %v, want %v", gotKnown, tc.wantKnown)
			}
		})
	}
}
