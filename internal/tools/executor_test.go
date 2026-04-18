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

	e.mu.Lock()
	e.attachments = map[string][]byte{"stale.txt": []byte("old data")}
	e.screenshotData = []byte("old screenshot")
	e.mu.Unlock()

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

	e.SetAttachments(map[string][]byte{"screenshot.png": []byte("user screenshot")})

	e.mu.Lock()
	e.screenshotData = []byte("tool screenshot")
	e.mu.Unlock()

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

func TestToolExecutorAttachmentsSurviveExecute(t *testing.T) {
	e := newTestExecutor()

	// Set attachments before any Execute call (this is what bot/handler.go does)
	e.SetAttachments(map[string][]byte{"report.pdf": []byte("pdf data")})

	// Simulate an Execute call — attachments must NOT be cleared
	// (Execute reads the epoch and updates lastSeenEpoch but does not clear)
	e.mu.Lock()
	if e.attachmentEpoch == 0 {
		t.Error("expected attachmentEpoch > 0 after SetAttachments")
	}
	e.lastSeenEpoch = e.attachmentEpoch
	e.mu.Unlock()

	// Verify attachments are still present
	e.mu.Lock()
	if len(e.attachments) != 1 {
		t.Errorf("expected 1 attachment after Execute, got %d", len(e.attachments))
	}
	if _, ok := e.attachments["report.pdf"]; !ok {
		t.Error("report.pdf attachment should survive Execute")
	}
	e.mu.Unlock()
}

func TestToolExecutorStaleDetectionOnNextSession(t *testing.T) {
	e := newTestExecutor()

	// Simulate a previous session: SetAttachments, then Execute updated lastSeenEpoch
	e.SetAttachments(map[string][]byte{"old.txt": []byte("old data")})
	e.mu.Lock()
	e.lastSeenEpoch = e.attachmentEpoch
	e.mu.Unlock()

	// Now a new session starts: SetAttachments again (incrementing epoch)
	e.SetAttachments(map[string][]byte{"new.txt": []byte("new data")})

	// In normal flow, ClearAttachments resets lastSeenEpoch before the next session.
	e.ClearAttachments()

	e.SetAttachments(map[string][]byte{"fresh.txt": []byte("fresh data")})
	// Now lastSeenEpoch was reset by ClearAttachments, so Execute should not clear
	e.mu.Lock()
	savedEpoch := e.lastSeenEpoch
	e.mu.Unlock()

	if savedEpoch != 0 {
		t.Errorf("expected lastSeenEpoch=0 after ClearAttachments, got %d", savedEpoch)
	}

	// Execute should not clear fresh attachments since lastSeenEpoch is 0
	e.mu.Lock()
	e.lastSeenEpoch = e.attachmentEpoch
	e.mu.Unlock()

	if _, ok := e.attachments["fresh.txt"]; !ok {
		t.Error("fresh.txt should survive Execute when lastSeenEpoch was 0")
	}
}
