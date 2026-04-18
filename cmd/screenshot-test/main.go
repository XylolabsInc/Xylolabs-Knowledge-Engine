package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/chromedp/chromedp"
)

func main() {
	url := "https://naver.com"
	if len(os.Args) > 1 {
		url = os.Args[1]
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(1280, 960),
		chromedp.Flag("disable-gpu", true),
		// Required in Docker containers without kernel sandboxing support.
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-software-rasterizer", true),
		chromedp.Flag("lang", "ko-KR,ko,en-US,en"),
		chromedp.Flag("font-render-hinting", "none"),
	)

	if chromePath := os.Getenv("CHROME_PATH"); chromePath != "" {
		opts = append(opts, chromedp.ExecPath(chromePath))
	}

	ctx := context.Background()

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	taskCtx, timeoutCancel := context.WithTimeout(taskCtx, 60*time.Second)
	defer timeoutCancel()

	var buf []byte
	if err := chromedp.Run(taskCtx,
		chromedp.Navigate(url),
		chromedp.Sleep(2*time.Second),
		chromedp.CaptureScreenshot(&buf),
	); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	outPath := "/tmp/chromedp-test.png"
	if err := os.WriteFile(outPath, buf, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("saved %s (%d bytes)\n", outPath, len(buf))
}
