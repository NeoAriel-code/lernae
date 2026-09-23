package domain

import "testing"

func TestMediumValid(t *testing.T) {
	tests := []struct {
		name   string
		medium Medium
		valid  bool
	}{
		{name: "game", medium: MediumGame, valid: true},
		{name: "video", medium: MediumVideo, valid: true},
		{name: "literature", medium: MediumLiterature, valid: true},
		{name: "audio", medium: MediumAudio, valid: true},
		{name: "edition format is not a broad medium", medium: "epub"},
		{name: "unknown", medium: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.medium.Valid(); got != tt.valid {
				t.Fatalf("Medium(%q).Valid() = %t, want %t", tt.medium, got, tt.valid)
			}
		})
	}
}
