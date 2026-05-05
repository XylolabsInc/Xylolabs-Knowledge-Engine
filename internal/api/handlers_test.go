package api

import (
	"testing"
)

func TestTruncateID(t *testing.T) {
	tests := []struct {
		id     string
		maxLen int
		want   string
	}{
		{"", 12, ""},
		{"a", 12, "a"},
		{"shortid", 12, "shortid"},
		{"exactly12ch", 12, "exactly12ch"},
		{"longerthan12characters", 12, "longerthan12"},
		{"abc123def456ghi789", 12, "abc123def456"},
	}

	for _, tt := range tests {
		got := truncateID(tt.id, tt.maxLen)
		if got != tt.want {
			t.Errorf("truncateID(%q, %d) = %q, want %q", tt.id, tt.maxLen, got, tt.want)
		}
	}
}

func TestTruncateIDExactBoundary(t *testing.T) {
	// Exactly 12 chars — should not truncate
	id := "012345678912"
	got := truncateID(id, 12)
	if got != id {
		t.Errorf("truncateID(%q, 12) = %q, want %q", id, got, id)
	}
}

func TestTruncateIDSHA256Length(t *testing.T) {
	// SHA256 hex = 64 chars
	id := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	got := truncateID(id, 12)
	if len(got) != 12 {
		t.Errorf("truncateID(SHA256, 12) length = %d, want 12", len(got))
	}
}
