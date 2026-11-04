package provider

import (
	"strings"
	"testing"
)

func TestNames(t *testing.T) {
	for _, tt := range []struct{ stored, fqdn string }{
		{"@", "example.com"},
		{"www", "www.example.com"},
		{"*.apps", "*.apps.example.com"},
		{"WWW", "www.example.com"},
	} {
		if got := recordFQDN(tt.stored, "example.com"); got != tt.fqdn {
			t.Errorf("recordFQDN(%q) = %q, want %q", tt.stored, got, tt.fqdn)
		}
	}
	for _, tt := range []struct{ fqdn, rel string }{
		{"example.com", "@"},
		{"example.com.", "@"},
		{"WWW.Example.com", "www"},
		{"*.apps.example.com", "*.apps"},
		{"a.b.example.com", "a.b"},
	} {
		if got := relativeName(tt.fqdn, "example.com"); got != tt.rel {
			t.Errorf("relativeName(%q) = %q, want %q", tt.fqdn, got, tt.rel)
		}
	}
	if z, ok := zoneFor("api.dev.example.com", []string{"example.com", "dev.example.com"}); !ok || z != "dev.example.com" {
		t.Errorf("zoneFor picks the longest match, got %q", z)
	}
	if _, ok := zoneFor("example.org", []string{"example.com"}); ok {
		t.Error("no zone for a foreign name")
	}
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"example.com", "www.example.com", "*.apps.example.com", "_sip._tcp.example.com", "extdns-a.wildcard.apps.example.com"} {
		if err := checkName(ok, "example.com"); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"extdns-a.*.apps.example.com", "a b.example.com", "a..example.com", strings.Repeat("x", 64) + ".example.com"} {
		if err := checkName(bad, "example.com"); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}

func TestTargets(t *testing.T) {
	tests := []struct {
		typ, target, content, canonical string
	}{
		{"A", "1.1.1.1", "1.1.1.1", "1.1.1.1"},
		{"A", "::ffff:1.1.1.1", "1.1.1.1", "1.1.1.1"},
		{"AAAA", "2606:4700:4700:0:0:0:0:1111", "2606:4700:4700::1111", "2606:4700:4700::1111"},
		{"CNAME", "Target.Example.NET.", "target.example.net.", "target.example.net"},
		{"CNAME", "target.example.net", "target.example.net.", "target.example.net"},
		{"TXT", "plain text", `"plain text"`, `"plain text"`},
		{"TXT", `"heritage=external-dns,external-dns/owner=x"`, `"heritage=external-dns,external-dns/owner=x"`, `"heritage=external-dns,external-dns/owner=x"`},
		{"TXT", `say "hi"`, `"say \"hi\""`, `"say \"hi\""`},
		{"TXT", `"a" "b"`, `"a" "b"`, `"a" "b"`},
		{"MX", "10 Mail.Example.com.", "10 mail.example.com.", "10 mail.example.com"},
		{"SRV", "10 5 5060 sip.example.com", "10 5 5060 sip.example.com.", "10 5 5060 sip.example.com"},
		{"CAA", `0 issue "letsencrypt.org"`, `0 issue "letsencrypt.org"`, `0 issue "letsencrypt.org"`},
		{"CAA", `0 ISSUE letsencrypt.org`, `0 issue "letsencrypt.org"`, `0 issue "letsencrypt.org"`},
	}
	for _, tt := range tests {
		c, err := contentFor("x.example.com", tt.typ, tt.target)
		if err != nil {
			t.Errorf("%s %q: %v", tt.typ, tt.target, err)
			continue
		}
		if c != tt.content {
			t.Errorf("contentFor(%s %q) = %q, want %q", tt.typ, tt.target, c, tt.content)
		}
		if got := canonicalTarget(tt.typ, tt.target); got != tt.canonical {
			t.Errorf("canonicalTarget(%s %q) = %q, want %q", tt.typ, tt.target, got, tt.canonical)
		}
		// What Hostinger returns must read back as the canonical target.
		if got, _ := recordTarget(tt.typ, c); got != tt.canonical {
			t.Errorf("recordTarget(%s %q) = %q, want %q", tt.typ, c, got, tt.canonical)
		}
	}
	for _, bad := range []struct{ typ, target string }{
		{"A", "2606:4700::1"}, {"AAAA", "1.1.1.1"}, {"A", "nope"}, {"MX", "mail.example.com"},
		{"MX", "x mail.example.com"}, {"SRV", "1 2 mail"}, {"CAA", "0 issue"}, {"CNAME", "a b"}, {"NS", "ns1.example.com"}, {"A", " "},
	} {
		if _, err := contentFor("x.example.com", bad.typ, bad.target); err == nil {
			t.Errorf("%s %q must be refused", bad.typ, bad.target)
		}
	}
}

func TestClampTTL(t *testing.T) {
	for in, want := range map[int64]int64{1: 60, 60: 60, 300: 300, 86400: 86400, 604800: 86400} {
		if got := clampTTL(in); got != want {
			t.Errorf("clampTTL(%d) = %d, want %d", in, got, want)
		}
	}
}
