package tools

import (
	"context"
	"fmt"
	"log/slog"
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
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-software-rasterizer", true),
		chromedp.Flag("lang", "ko-KR,ko,en-US,en"),
		chromedp.Flag("font-render-hinting", "none"),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	taskCtx, timeoutCancel := context.WithTimeout(taskCtx, 30*time.Second)
	defer timeoutCancel()

	// Inject Google Fonts CJK web font to ensure Korean/Japanese/Chinese text
	// renders correctly even when system fonts are unavailable (e.g. snap Chromium).
	const cjkFontCSS = `
		var style = document.createElement('style');
		style.textContent = '@import url("https://fonts.googleapis.com/css2?family=Noto+Sans+KR:wght@400;700&family=Noto+Sans+JP:wght@400;700&family=Noto+Sans+SC:wght@400;700&display=swap"); * { font-family: "Noto Sans KR", "Noto Sans JP", "Noto Sans SC", sans-serif !important; }';
		document.head.appendChild(style);
	`

	var buf []byte
	actions := []chromedp.Action{
		chromedp.Navigate(url),
		chromedp.Sleep(1 * time.Second),
		chromedp.Evaluate(cjkFontCSS, nil),
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
