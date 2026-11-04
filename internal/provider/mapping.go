package provider

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"sigs.k8s.io/external-dns/endpoint"

	"github.com/hayone1/external-dns-hostinger-webhook/internal/hostinger"
)

// supportedTypes are the record types the webhook manages. ALIAS is a
// Hostinger extension that external-dns cannot ask for; NS and SOA belong
// to Hostinger.
var supportedTypes = map[string]bool{
	hostinger.TypeA:     true,
	hostinger.TypeAAAA:  true,
	hostinger.TypeCNAME: true,
	hostinger.TypeTXT:   true,
	hostinger.TypeMX:    true,
	hostinger.TypeSRV:   true,
	hostinger.TypeCAA:   true,
}

func supportedType(t string) bool { return supportedTypes[t] }

// normalizeName lower-cases a DNS name and removes the trailing dot.
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// recordFQDN turns the name Hostinger stores ("www", "*.apps" or "@") into
// a full name. Hostinger always stores names relative to the zone.
func recordFQDN(name, zone string) string {
	n := normalizeName(name)
	if n == "" || n == hostinger.Apex {
		return zone
	}
	return n + "." + zone
}

// relativeName is the opposite of recordFQDN. Hostinger accepts full names
// on PUT, but DELETE only matches the stored, relative spelling, so the
// webhook always sends relative names.
func relativeName(fqdn, zone string) string {
	n := normalizeName(fqdn)
	if n == zone {
		return hostinger.Apex
	}
	return strings.TrimSuffix(n, "."+zone)
}

// zoneFor returns the zone that owns name, picking the longest match so that
// a delegated child zone wins over its parent. zones must be normalized.
func zoneFor(name string, zones []string) (string, bool) {
	best := ""
	for _, z := range zones {
		if (name == z || strings.HasSuffix(name, "."+z)) && len(z) > len(best) {
			best = z
		}
	}
	return best, best != ""
}

// checkName reports why Hostinger would refuse a record name, or nil.
// Hostinger allows letters, digits, "-" and "_" in labels, and "*" only as
// the whole first label.
func checkName(fqdn, zone string) error {
	rel := relativeName(fqdn, zone)
	if rel == hostinger.Apex {
		return nil
	}
	for i, label := range strings.Split(rel, ".") {
		if label == "*" {
			if i == 0 {
				continue
			}
			return fmt.Errorf("%s: Hostinger only allows \"*\" as the first label of a name", fqdn)
		}
		if label == "" || len(label) > 63 {
			return fmt.Errorf("%s: empty or too long label %q", fqdn, label)
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
				return fmt.Errorf("%s: Hostinger does not allow %q in a name", fqdn, r)
			}
		}
	}
	return nil
}

// recordTarget turns one Hostinger value into an external-dns target.
func recordTarget(recordType, content string) (string, bool) {
	c := strings.TrimSpace(content)
	if c == "" {
		return "", false
	}
	switch recordType {
	case hostinger.TypeA, hostinger.TypeAAAA:
		if ip, err := netip.ParseAddr(c); err == nil {
			return ip.Unmap().String(), true
		}
		return c, true
	case hostinger.TypeCNAME:
		return normalizeName(c), true
	case hostinger.TypeTXT:
		// TXT values are passed through untouched so that external-dns
		// ownership records round-trip exactly. Hostinger stores them
		// with quotes.
		return content, true
	case hostinger.TypeMX, hostinger.TypeSRV:
		f := strings.Fields(c)
		if len(f) == 0 {
			return "", false
		}
		f[len(f)-1] = normalizeName(f[len(f)-1])
		return strings.Join(f, " "), true
	case hostinger.TypeCAA:
		if flag, tag, value, err := parseCAA(c); err == nil {
			return fmt.Sprintf("%d %s \"%s\"", flag, tag, value), true
		}
		return c, true
	}
	return "", false
}

// contentFor turns one external-dns target into the value Hostinger stores.
// recordTarget(contentFor(t)) is the canonical form of t.
func contentFor(fqdn, recordType, target string) (string, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", fmt.Errorf("%s %s: empty target", recordType, fqdn)
	}
	switch recordType {
	case hostinger.TypeA, hostinger.TypeAAAA:
		ip, err := netip.ParseAddr(t)
		if err != nil {
			return "", fmt.Errorf("%s %s: %q is not an IP address", recordType, fqdn, target)
		}
		ip = ip.Unmap()
		if (recordType == hostinger.TypeA) != ip.Is4() {
			return "", fmt.Errorf("%s %s: %q is the wrong IP version for this record type", recordType, fqdn, target)
		}
		return ip.String(), nil
	case hostinger.TypeCNAME:
		if strings.ContainsAny(t, " \"") {
			return "", fmt.Errorf("CNAME %s: %q is not a host name", fqdn, target)
		}
		return normalizeName(t) + ".", nil
	case hostinger.TypeTXT:
		return quoteTXT(t), nil
	case hostinger.TypeMX:
		f := strings.Fields(t)
		if len(f) != 2 {
			return "", fmt.Errorf("MX %s: target %q must look like \"10 mail.example.com\"", fqdn, target)
		}
		if _, err := strconv.ParseUint(f[0], 10, 16); err != nil {
			return "", fmt.Errorf("MX %s: bad priority in %q", fqdn, target)
		}
		return f[0] + " " + normalizeName(f[1]) + ".", nil
	case hostinger.TypeSRV:
		f := strings.Fields(t)
		if len(f) != 4 {
			return "", fmt.Errorf("SRV %s: target %q must look like \"10 5 443 host.example.com\"", fqdn, target)
		}
		for _, n := range f[:3] {
			if _, err := strconv.ParseUint(n, 10, 16); err != nil {
				return "", fmt.Errorf("SRV %s: bad number in %q", fqdn, target)
			}
		}
		return strings.Join(f[:3], " ") + " " + normalizeName(f[3]) + ".", nil
	case hostinger.TypeCAA:
		flag, tag, value, err := parseCAA(t)
		if err != nil {
			return "", fmt.Errorf("CAA %s: %w", fqdn, err)
		}
		return fmt.Sprintf("%d %s \"%s\"", flag, tag, value), nil
	}
	return "", fmt.Errorf("record type %s is not supported by this webhook", recordType)
}

// canonicalTarget gives a form of target that is safe to compare: two
// targets that Hostinger stores the same way get the same canonical form.
func canonicalTarget(recordType, target string) string {
	if c, err := contentFor("", recordType, target); err == nil {
		if t, ok := recordTarget(recordType, c); ok {
			return t
		}
	}
	return strings.TrimSpace(target)
}

// quoteTXT wraps a TXT value in quotes, the way Hostinger stores it. A
// value that is already quoted (including several quoted strings, like
// `"part one" "part two"`) is left alone. Quotes inside the value are
// escaped, because Hostinger refuses a bare quote.
func quoteTXT(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v
	}
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}

// parseCAA reads `0 issue "letsencrypt.org"`. The quotes are optional.
func parseCAA(target string) (int64, string, string, error) {
	parts := strings.SplitN(strings.TrimSpace(target), " ", 3)
	if len(parts) != 3 {
		return 0, "", "", fmt.Errorf("target %q must look like `0 issue \"letsencrypt.org\"`", target)
	}
	flag, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || flag < 0 || flag > 255 {
		return 0, "", "", fmt.Errorf("bad flag in %q", target)
	}
	tag := strings.ToLower(strings.TrimSpace(parts[1]))
	if tag == "" {
		return 0, "", "", fmt.Errorf("missing tag in %q", target)
	}
	return flag, tag, unquote(strings.TrimSpace(parts[2])), nil
}

// unquote removes one pair of surrounding double quotes, if present.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// clampTTL keeps a TTL inside the range Hostinger accepts.
func clampTTL(ttl int64) int64 {
	return min(max(ttl, hostinger.MinTTL), hostinger.MaxTTL)
}

// endpointKey identifies one record set.
type endpointKey struct {
	name       string
	recordType string
}

func keyOf(ep *endpoint.Endpoint) endpointKey {
	return endpointKey{name: normalizeName(ep.DNSName), recordType: ep.RecordType}
}
