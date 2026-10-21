package integration

import (
	"slices"
	"strings"
	"testing"
)

func TestNetworkIsolation(t *testing.T) {
	h := helper{image: "test-image", rootMode: "tmpfs", isolateNetwork: true}
	ifaces := strings.Split(h.probe(t, "interfaces"), "\n")
	if !slices.Contains(ifaces, "lo") {
		t.Errorf("no loopback interface, got %q", ifaces)
	}
	for _, iface := range ifaces {
		// Some container runtimes bring up extra interfaces related to the
		// network bridge in the sandbox, so check for the absence of external
		// interfaces rather than for only lo being present.
		for _, prefix := range []string{"eth", "en", "wl", "veth"} {
			if strings.HasPrefix(iface, prefix) {
				t.Errorf("external interface %q present, got %q", iface, ifaces)
			}
		}
	}
}
