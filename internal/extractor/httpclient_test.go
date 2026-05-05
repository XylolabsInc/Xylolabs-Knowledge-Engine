package extractor

import (
	"context"
	"net"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr string
		want bool
	}{
		{addr: "8.8.8.8", want: true},
		{addr: "127.0.0.1", want: false},
		{addr: "10.0.0.1", want: false},
		{addr: "169.254.1.1", want: false},
		{addr: "::1", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			if got := isPublicIP(net.ParseIP(tt.addr)); got != tt.want {
				t.Fatalf("isPublicIP(%s) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestValidateDialTarget(t *testing.T) {
	t.Parallel()

	publicLookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	privateLookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.2")}}, nil
	}

	if err := validateDialTarget(context.Background(), "example.com:443", publicLookup); err != nil {
		t.Fatalf("validateDialTarget() unexpected error for public host: %v", err)
	}
	if err := validateDialTarget(context.Background(), "example.com:443", privateLookup); err == nil {
		t.Fatal("expected private lookup result to be rejected")
	}
	if err := validateDialTarget(context.Background(), "127.0.0.1:8080", publicLookup); err == nil {
		t.Fatal("expected loopback IP target to be rejected")
	}
}
