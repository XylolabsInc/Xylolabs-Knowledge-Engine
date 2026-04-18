package tools

import (
	"log/slog"
	"testing"
)

func newTestExecutor() *ToolExecutor {
	return NewToolExecutor(nil, nil, "", nil, nil, nil, slog.Default())
}

func TestToolExecutorStaleAttachmentCleanup(t *testing.T) {
	e := newTestExecutor()

	// Manually set stale attachments
	e.mu.Lock()
	e.attachments = map[string][]byte{"stale.txt": []byte("old data")}
	e.screenshotData = []byte("old screenshot")
	e.mu.Unlock()

	// Calling SetAttachments should replace the old ones
	e.SetAttachments(map[string][]byte{"new.txt": []byte("new data")})

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.attachments["stale.txt"]; ok {
		t.Error("stale attachment should have been replaced")
	}
	if _, ok := e.attachments["new.txt"]; !ok {
		t.Error("new attachment should exist")
	}
}

func TestToolExecutorPopScreenshotEmpty(t *testing.T) {
	e := newTestExecutor()
	_, ok := e.PopScreenshot()
	if ok {
		t.Error("PopScreenshot on empty executor should return false")
	}
}

func TestToolExecutorSetClearAttachments(t *testing.T) {
	e := newTestExecutor()

	e.SetAttachments(map[string][]byte{"file.txt": []byte("data")})
	e.mu.Lock()
	if len(e.attachments) != 1 {
		t.Errorf("expected 1 attachment, got %d", len(e.attachments))
	}
	e.mu.Unlock()

	e.ClearAttachments()
	e.mu.Lock()
	if len(e.attachments) != 0 {
		t.Errorf("expected 0 attachments after clear, got %d", len(e.attachments))
	}
	e.mu.Unlock()
}

func TestToolExecutorScreenshotIsolation(t *testing.T) {
	e := newTestExecutor()

	// Set a user attachment named "screenshot.png"
	e.SetAttachments(map[string][]byte{"screenshot.png": []byte("user screenshot")})

	// Simulate screenshot_url setting screenshotData
	e.mu.Lock()
	e.screenshotData = []byte("tool screenshot")
	e.mu.Unlock()

	// User's attachment should still be there
	e.mu.Lock()
	if _, ok := e.attachments["screenshot.png"]; !ok {
		t.Error("user's screenshot.png attachment should be preserved")
	}
	e.mu.Unlock()

	// PopScreenshot should return the tool screenshot
	data, ok := e.PopScreenshot()
	if !ok {
		t.Fatal("PopScreenshot should return true")
	}
	if string(data) != "tool screenshot" {
		t.Errorf("PopScreenshot returned %q, want 'tool screenshot'", string(data))
	}

	// User's attachment should still be there after PopScreenshot
	e.mu.Lock()
	if _, ok := e.attachments["screenshot.png"]; !ok {
		t.Error("user's screenshot.png should still exist after PopScreenshot")
	}
	e.mu.Unlock()
}
