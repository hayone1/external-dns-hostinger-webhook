package hostinger

// Record types the Hostinger DNS API knows.
const (
	TypeA     = "A"
	TypeAAAA  = "AAAA"
	TypeCNAME = "CNAME"
	TypeALIAS = "ALIAS"
	TypeMX    = "MX"
	TypeTXT   = "TXT"
	TypeNS    = "NS"
	TypeSOA   = "SOA"
	TypeSRV   = "SRV"
	TypeCAA   = "CAA"
)

// Apex is the record name Hostinger uses for the zone itself.
const Apex = "@"

// TTL limits of the Hostinger DNS API. Lower or higher values get HTTP 422.
const (
	MinTTL = 60
	MaxTTL = 86400
)

// ProtectedTypes are record types the webhook never writes or deletes.
// Hostinger manages NS and SOA itself.
var ProtectedTypes = map[string]bool{TypeNS: true, TypeSOA: true}

// RRset is one record set: a name, a type, a TTL and a list of values.
// Hostinger has no record IDs; a set is addressed by name and type.
type RRset struct {
	// Name is relative to the zone: "www", "*.apps" or "@" for the apex.
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	TTL     int64    `json:"ttl"`
	Records []Record `json:"records"`
}

// Record is one value of an RRset.
type Record struct {
	Content string `json:"content"`
	// IsDisabled marks a value that was turned off in hPanel. Disabled
	// values are stored but not served.
	IsDisabled bool `json:"is_disabled,omitempty"`
}

// Filter selects RRsets to delete. Name must be spelled exactly as the API
// stores it (relative, lower case); anything else silently matches nothing.
type Filter struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Domain is one entry of the domain portfolio.
type Domain struct {
	ID     int64   `json:"id"`
	Domain *string `json:"domain"`
	Type   string  `json:"type"`
	Status string  `json:"status"`
}

// DomainDetails is the answer of GET /api/domains/v1/portfolio/{domain}.
type DomainDetails struct {
	Domain      string            `json:"domain"`
	Status      string            `json:"status"`
	NameServers map[string]string `json:"name_servers"`
}

// HostingerNameServers are the name servers a domain must use so that
// Hostinger serves its DNS records.
var HostingerNameServers = []string{"ns1.dns-parking.com", "ns2.dns-parking.com"}

type updateRequest struct {
	Overwrite bool    `json:"overwrite"`
	Zone      []RRset `json:"zone"`
}

type deleteRequest struct {
	Filters []Filter `json:"filters"`
}

// apiErrorBody is the shape of every error answer:
//
//	{"message": "...", "errors": {"field": ["..."]}, "correlation_id": "..."}
type apiErrorBody struct {
	Message       string              `json:"message"`
	Errors        map[string][]string `json:"errors"`
	CorrelationID string              `json:"correlation_id"`
}
