package tools

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// blockedMetadataHostnames are known cloud metadata endpoints that must never
// be reached from a headless browser.
var blockedMetadataHostnames = map[string]bool{
	"metadata.google.internal": true,
}

// validateScreenshotURL ensures the given URL is safe for the headless browser
// to navigate to. It rejects non-HTTP schemes, private/loopback/link-local IPs,
// and known cloud metadata endpoints.
func validateScreenshotURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("screenshot: invalid URL %q: %w", rawURL, err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("screenshot: unsupported scheme %q (only http and https are allowed)", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("screenshot: URL has no hostname")
	}

	// Block known cloud metadata hostnames.
	if blockedMetadataHostnames[strings.ToLower(host)] {
		return fmt.Errorf("screenshot: blocked metadata hostname %q", host)
	}

	// If the host is an IP literal, validate it directly.
	if ip := net.ParseIP(host); ip != nil {
		if ip.String() == "169.254.169.254" {
			return fmt.Errorf("screenshot: blocked metadata IP 169.254.169.254")
		}
		if !isScreenshotPublicIP(ip) {
			return fmt.Errorf("screenshot: refusing private or loopback address %q", host)
		}
		return nil
	}

	// Resolve the hostname and verify every resulting IP is public.
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("screenshot: resolve host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("screenshot: host %q resolved to no addresses", host)
	}

	for _, ipAddr := range ips {
		if ipAddr.IP.String() == "169.254.169.254" {
			return fmt.Errorf("screenshot: host %q resolves to blocked metadata IP 169.254.169.254", host)
		}
		if !isScreenshotPublicIP(ipAddr.IP) {
			return fmt.Errorf("screenshot: refusing non-public address for host %q", host)
		}
	}

	return nil
}

// isScreenshotPublicIP reports whether ip is a publicly routable address.
// It mirrors the isPublicIP check in internal/extractor/httpclient.go.
func isScreenshotPublicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	return !addr.IsLoopback() &&
		!addr.IsPrivate() &&
		!addr.IsLinkLocalUnicast() &&
		!addr.IsLinkLocalMulticast() &&
		!addr.IsMulticast() &&
		!addr.IsUnspecified()
}

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
	if err := validateScreenshotURL(ctx, url); err != nil {
		return nil, err
	}

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
