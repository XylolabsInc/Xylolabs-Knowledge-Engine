package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The unit must start after the network is up and names resolve.
func TestUnitWaitsForNetworkAndDNS(t *testing.T) {
	unit, err := os.ReadFile(filepath.Join("..", "..", "configs", "xylolabs-kb.service"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(unit)
	for _, want := range []string{"network-online.target", "nss-lookup.target"} {
		if !strings.Contains(s, "After=") || !strings.Contains(s, want) {
			t.Errorf("unit does not order itself after %s", want)
		}
	}
	if !strings.Contains(s, "Wants=network-online.target") {
		t.Error("unit does not pull in network-online.target")
	}
}
