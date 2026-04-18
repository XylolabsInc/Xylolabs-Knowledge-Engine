package extractor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

type ipLookupFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

func NewRestrictedHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if err := validateDialTarget(ctx, address, net.DefaultResolver.LookupIPAddr); err != nil {
			return nil, err
		}
		return dialer.DialContext(ctx, network, address)
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

func validateDialTarget(ctx context.Context, address string, lookup ipLookupFunc) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("extractor: invalid dial target %q: %w", address, err)
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return fmt.Errorf("extractor: invalid empty host")
	}

	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("extractor: refusing private or loopback address %q", host)
		}
		return nil
	}

	ips, err := lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("extractor: resolve host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("extractor: host %q resolved to no addresses", host)
	}

	for _, ip := range ips {
		if !isPublicIP(ip.IP) {
			return fmt.Errorf("extractor: refusing non-public address for host %q", host)
		}
	}

	return nil
}

func isPublicIP(ip net.IP) bool {
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
