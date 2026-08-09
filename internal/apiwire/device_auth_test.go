package apiwire

import "testing"

func TestNormalizePublicURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty is allowed", "", "", false},
		{"plain https origin", "https://example.com", "https://example.com", false},
		{"uppercase host is lowercased", "https://EXAMPLE.Com", "https://example.com", false},
		{"path is stripped", "https://example.com/path/x", "https://example.com", false},
		{"query is stripped", "https://example.com?x=1", "https://example.com", false},
		{"fragment is stripped", "https://example.com#f", "https://example.com", false},
		{"trailing slash is stripped", "https://example.com/", "https://example.com", false},
		{"port is preserved", "https://example.com:8443", "https://example.com:8443", false},
		{"IPv6 with brackets round-trips", "https://[::1]:8080", "https://[::1]:8080", false},
		{"IPv6 without port keeps brackets", "https://[2001:db8::1]", "https://[2001:db8::1]", false},
		{"leading/trailing whitespace is trimmed", "  https://example.com  ", "https://example.com", false},
		{"http scheme is rejected", "http://example.com", "", true},
		{"no scheme is rejected", "example.com", "", true},
		{"scheme-only is rejected", "https://", "", true},
		{"port-only host is rejected", "https://:443", "", true},
		{"userinfo does not become host", "https://user:pw@", "", true},
		// Adversarial authority forms that url.URL's own Hostname/Port
		// would silently mangle. splitAuthority must reject these outright.
		{"bracket-less IPv6 is rejected", "https://2001:db8::1", "", true},
		{"multi-colon authority is rejected", "https://example.com:80:443", "", true},
		{"bracketed non-IPv6 host is rejected", "https://[not-ipv6]:8080", "", true},
		{"userinfo before host is rejected", "https://alice@example.com", "", true},
		{"bracketed IPv6 with port round-trips", "https://[fe80::1]:8080", "https://[fe80::1]:8080", false},
		{"bracketed IPv6 without port keeps brackets", "https://[fe80::1]", "https://[fe80::1]", false},
		// Numeric port range: url.Parse accepts these as syntactically
		// valid, but they can never resolve to a real TCP endpoint —
		// reject at normalize time so canonical_url does not promise a
		// bogus origin.
		{"port zero is rejected", "https://example.com:0", "", true},
		{"port above max is rejected", "https://example.com:65536", "", true},
		{"port at max is accepted", "https://example.com:65535", "https://example.com:65535", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePublicURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("NormalizePublicURL(%q) = %q, nil; want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Errorf("NormalizePublicURL(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizePublicURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
