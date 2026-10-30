package ui

import "testing"

// m2-S5: the invite label must only claim a network it can see: the
// tailnet (100.64/10) and private LAN addresses. A public IP or a
// hostname gets no label — "local lan" would be a lie there.
func TestInviteLabelOnlyClaimsRealNetworks(t *testing.T) {
	cases := []struct{ addr, want string }{
		{"100.89.11.31:8090", "(tailscale)"},
		{"100.64.0.1:8090", "(tailscale)"},
		{"100.127.255.254:8090", "(tailscale)"},
		{"192.168.1.181:8090", "(local lan)"},
		{"10.0.0.5:8090", "(local lan)"},
		{"127.0.0.1:8090", "(local lan)"},
		{"http://192.168.1.181:8090", "(local lan)"},
		// the docker range is not a home LAN: no claim
		{"172.16.4.4:8090", ""},
		{"172.31.255.255:8090", ""},
		// a public address: no claim
		{"203.0.113.7:8090", ""},
		{"8.8.8.8:8090", ""},
		// a hostname: no claim
		{"watchparty.example.com:8090", ""},
		// outside the tailnet range
		{"100.63.255.255:8090", ""},
		{"100.128.0.1:8090", ""},
	}
	for _, c := range cases {
		if got := inviteLabel(c.addr); got != c.want {
			t.Errorf("inviteLabel(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}
