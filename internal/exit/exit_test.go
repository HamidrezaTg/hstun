package exit

import (
	"net"
	"testing"
)

func TestForbidden(t *testing.T) {
	own := []net.IP{net.ParseIP("203.0.113.9")}
	deny := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.1.1",
		"100.64.0.1", "100.118.30.55", "0.0.0.0", "224.0.0.1", "255.255.255.255",
		"fe80::1", "fc00::1", "203.0.113.9",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::7f00:1", "64:ff9b::a00:1", "2002:7f00:1::1", "2002:c0a8:101::1",
	}
	for _, s := range deny {
		if !forbidden(net.ParseIP(s), own) {
			t.Errorf("%s should be refused", s)
		}
	}
	allow := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "100.128.0.1", "64:ff9b::808:808", "2002:808:808::1"}
	for _, s := range allow {
		if forbidden(net.ParseIP(s), own) {
			t.Errorf("%s should be allowed", s)
		}
	}
}
