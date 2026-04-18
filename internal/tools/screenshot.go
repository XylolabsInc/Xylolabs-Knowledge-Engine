package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/chromedp/chromedp"
)

// Screenshotter captures web page screenshots using headless Chromium.
type Screenshotter struct {
	logger *slog.Logger
}

// NewScreenshotter creates a new Screenshotter.
func NewScreenshotter(logger *slog.Logger) *Screenshotter {
	return &Screenshotter{logger: logger.With("component", "screenshotter")}
}

// Capture takes a screenshot of the given URL and returns PNG bytes.
// width/height set the viewport size in pixels (default 1280x960).
// fullPage captures the full scrollable page when true, otherwise only the viewport.
func (s *Screenshotter) Capture(ctx context.Context, url string, width, height int, fullPage bool) ([]byte, error) {
	if width <= 0 {
		width = 1280
	}
	if height <= 0 {
		height = 960
	}

	s.logger.Info("capturing screenshot", "url", url, "width", width, "height", height, "full_page", fullPage)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(width, height),
		chromedp.Flag("disable-gpu", true),
		// Required in Docker containers without kernel sandboxing support.
		// The container runs as non-root with minimal capabilities (see Dockerfile).
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-software-rasterizer", true),
		chromedp.Flag("lang", "ko-KR,ko,en-US,en"),
		chromedp.Flag("font-render-hinting", "none"),
	)

	// Use a custom Chrome binary if CHROME_PATH is set (e.g. to bypass snap sandbox for CJK fonts).
	if chromePath := os.Getenv("CHROME_PATH"); chromePath != "" {
		opts = append(opts, chromedp.ExecPath(chromePath))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	taskCtx, timeoutCancel := context.WithTimeout(taskCtx, 60*time.Second)
	defer timeoutCancel()

	var buf []byte
	actions := []chromedp.Action{
		chromedp.Navigate(url),
		chromedp.Sleep(2 * time.Second),
	}

	if fullPage {
		actions = append(actions, chromedp.FullScreenshot(&buf, 90))
	} else {
		actions = append(actions, chromedp.CaptureScreenshot(&buf))
	}

	if err := chromedp.Run(taskCtx, actions...); err != nil {
		return nil, fmt.Errorf("capture screenshot: %w", err)
	}

	s.logger.Info("screenshot captured", "url", url, "size_bytes", len(buf))
	return buf, nil
}
