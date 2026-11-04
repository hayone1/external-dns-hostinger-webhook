// Package hostingertest is an in-memory fake of the Hostinger DNS API for
// tests.
//
// It copies the behavior seen on the real API (developers.hostinger.com,
// probed on 2026-11-04, see docs/hostinger-api.md):
//
//   - a zone is a list of RRsets (name, type, ttl, records); there are no
//     record IDs and no paging
//   - names are stored relative and lower case; "@" is the apex
//   - PUT accepts full names ("www.zone", "www.zone.", "zone") and turns
//     them into relative names; an empty name is HTTP 422
//   - "*" is only allowed as the first label ("*.x" works, "a.*.x" is 422)
//   - PUT with overwrite=true replaces the RRsets it names; overwrite=false
//     appends values and updates the TTL, and a value that already exists
//     is 422 [DNS:4008]
//   - a PUT is all or nothing: one bad RRset fails the whole call
//   - TTL must be 60 to 86400; a missing TTL becomes 1800
//   - an RRset needs at least one value; duplicates are 422
//   - a CNAME has one value and no other type may share its name
//   - content is rewritten: CNAME, ALIAS, MX and SRV targets get a
//     trailing dot, TXT gets quotes, a CAA value gets quotes, AAAA is
//     written in its shortest form
//   - DELETE removes whole RRsets by exact stored name and type; a filter
//     that matches nothing is still HTTP 200, even if the name is spelled
//     differently (full name, upper case) from what is stored
//   - a domain that is not in the account is HTTP 403 [DNS:4002]
//   - a bad token is HTTP 401 {"message":"Unauthenticated."}
//   - every answer carries RateLimit headers
//   - NS and SOA never show up in GET; SOA and NS below the apex cannot be
//     written (HTTP 422 [DNS:4005])
//   - two writes to one zone at the same moment can lose one of them while
//     both answer 200 (not copied here: the webhook never writes in
//     parallel)
//
// Reset and snapshot restore are not implemented. A call to either is
// counted (see ForbiddenCalls) and refused, so tests can prove the webhook
// never makes one.
package hostingertest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Token is the API token the fake accepts.
const Token = "test-token"

// DefaultApexIP is the parking address a new zone points its apex to, like
// a fresh zone on the real API.
const DefaultApexIP = "2.57.91.91"

var validTypes = map[string]bool{
	"A": true, "AAAA": true, "CNAME": true, "ALIAS": true, "MX": true,
	"TXT": true, "NS": true, "SOA": true, "SRV": true, "CAA": true,
}

// RRset is a stored record set.
type RRset struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	TTL     int64    `json:"ttl"`
	Records []Record `json:"records"`
}

// Record is one stored value.
type Record struct {
	Content    string `json:"content"`
	IsDisabled bool   `json:"is_disabled"`
}

// Values returns the contents of the set's values.
func (s RRset) Values() []string {
	out := make([]string, 0, len(s.Records))
	for _, r := range s.Records {
		out = append(out, r.Content)
	}
	return out
}

// Fault makes the next matching requests fail.
type Fault struct {
	Method       string      `json:"method,omitempty"`       // empty matches any method
	Path         string      `json:"path,omitempty"`         // substring of the URL path; empty matches any path
	BodyContains string      `json:"bodyContains,omitempty"` // substring of the request body; empty matches any body
	Status       int         `json:"status"`
	Count        int         `json:"count"`
	Header       http.Header `json:"header,omitempty"`
}

type zone struct {
	sets        []*RRset
	inPortfolio bool
	status      string
	nameServers []string
}

// Server is the fake API.
type Server struct {
	// Server is only set by New. NewHandler leaves it nil.
	*httptest.Server

	handler http.Handler
	mu      sync.Mutex
	zones   map[string]*zone
	order   []string
	faults  []*Fault
	calls   map[string]int
	corrSeq int

	// rate limit: at most limit requests per window; 0 means no limit.
	limit       int
	window      time.Duration
	windowStart time.Time
	used        int
	now         func() time.Time
}

// New starts a fake server on a random local port. Close it when done.
func New() *Server {
	s := NewHandler()
	s.Server = httptest.NewServer(s.handler)
	return s
}

// NewHandler creates the fake without starting a server. Serve Handler()
// yourself, for example in a container for end-to-end tests.
//
// Besides the Hostinger API it serves test-only endpoints:
//
//	POST   /_fake/faults   add a Fault (JSON body)
//	DELETE /_fake/faults   remove all faults
//	GET    /_fake/calls    call counters as JSON
//	GET    /healthz        "ok"
func NewHandler() *Server {
	s := &Server{
		zones:  map[string]*zone{},
		calls:  map[string]int{},
		window: time.Minute,
		now:    time.Now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_fake/faults", s.addFault)
	mux.HandleFunc("DELETE /_fake/faults", func(w http.ResponseWriter, _ *http.Request) {
		s.ClearFaults()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /_fake/calls", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		c := maps(s.calls)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, c)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })

	mux.HandleFunc("GET /api/domains/v1/portfolio", s.api("listDomains", s.listDomains))
	mux.HandleFunc("GET /api/domains/v1/portfolio/{domain}", s.api("getDomain", s.getDomain))
	mux.HandleFunc("GET /api/dns/v1/zones/{domain}", s.api("getZone", s.getZone))
	mux.HandleFunc("PUT /api/dns/v1/zones/{domain}", s.api("updateZone", s.updateZone))
	mux.HandleFunc("DELETE /api/dns/v1/zones/{domain}", s.api("deleteRRsets", s.deleteRRsets))
	mux.HandleFunc("POST /api/dns/v1/zones/{domain}/validate", s.api("validateZone", s.validateZone))
	mux.HandleFunc("GET /api/dns/v1/snapshots/{domain}", s.api("listSnapshots", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{})
	}))
	// Calls the webhook must never make.
	forbidden := func(op string) http.HandlerFunc {
		return s.api(op, func(w http.ResponseWriter, _ *http.Request) {
			s.mu.Lock()
			s.calls["forbidden"]++
			s.mu.Unlock()
			s.writeError(w, http.StatusForbidden, "fake: "+op+" must never be called by the webhook", nil)
		})
	}
	mux.HandleFunc("POST /api/dns/v1/zones/{domain}/reset", forbidden("resetZone"))
	mux.HandleFunc("POST /api/dns/v1/snapshots/{domain}/{id}/restore", forbidden("restoreSnapshot"))
	s.handler = mux
	return s
}

// Handler returns the HTTP handler of the fake API.
func (s *Server) Handler() http.Handler { return s.handler }

// AddZone creates a domain with a zone that holds the default records of a
// fresh Hostinger zone: "@ A 2.57.91.91" (TTL 50) and "www CNAME <zone>.".
func (s *Server) AddZone(name string) {
	s.addZone(name, true)
	s.Seed(name, RRset{Name: "@", Type: "A", TTL: 50, Records: []Record{{Content: DefaultApexIP}}})
	s.Seed(name, RRset{Name: "www", Type: "CNAME", TTL: 300, Records: []Record{{Content: name + "."}}})
}

// AddBareZone creates a domain with an empty zone.
func (s *Server) AddBareZone(name string) { s.addZone(name, true) }

// AddForeignZone creates an empty zone that is not in the domain portfolio,
// like a domain registered elsewhere whose DNS is hosted at Hostinger.
func (s *Server) AddForeignZone(name string) { s.addZone(name, false) }

func (s *Server) addZone(name string, inPortfolio bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.ToLower(name)
	if _, ok := s.zones[name]; ok {
		return
	}
	s.order = append(s.order, name)
	s.zones[name] = &zone{inPortfolio: inPortfolio, status: "active", nameServers: []string{"ns1.dns-parking.com", "ns2.dns-parking.com"}}
}

// SetDomainStatus changes the status the portfolio reports for a domain,
// for example "expired". Its zone can still be read and written.
func (s *Server) SetDomainStatus(domain, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if z := s.zones[domain]; z != nil {
		z.status = status
	}
}

// SetNameServers changes the name servers the portfolio reports for a domain.
func (s *Server) SetNameServers(domain string, ns ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if z := s.zones[domain]; z != nil {
		z.nameServers = ns
	}
}

// Seed stores an RRset as-is, without validation, replacing a set with the
// same name and type.
func (s *Server) Seed(domain string, set RRset) {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := s.zones[domain]
	if z == nil {
		panic("hostingertest: unknown zone " + domain)
	}
	c := set
	c.Records = slices.Clone(set.Records)
	if i := z.find(set.Name, set.Type); i >= 0 {
		z.sets[i] = &c
		return
	}
	z.sets = append(z.sets, &c)
}

// RRsets returns a copy of the RRsets in a zone.
func (s *Server) RRsets(domain string) []RRset {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := s.zones[domain]
	if z == nil {
		return nil
	}
	out := make([]RRset, 0, len(z.sets))
	for _, r := range z.sets {
		c := *r
		c.Records = slices.Clone(r.Records)
		out = append(out, c)
	}
	return out
}

// Inject adds a fault.
func (s *Server) Inject(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &f)
}

// ClearFaults removes every fault, as if the outage ended.
func (s *Server) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
}

// SetRateLimit makes the fake answer HTTP 429 after limit requests in one
// window, like the real API (90 per 60 seconds). 0 turns the limit off,
// which is the default.
func (s *Server) SetRateLimit(limit int, window time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limit, s.window, s.used, s.windowStart = limit, window, 0, time.Time{}
}

// Calls returns how often an operation was called: listDomains, getDomain,
// getZone, updateZone, validateZone, deleteRRsets.
func (s *Server) Calls(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

// MutatingCalls returns the number of PUT and DELETE calls on zones.
func (s *Server) MutatingCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls["updateZone"] + s.calls["deleteRRsets"]
}

// ForbiddenCalls returns how often a zone reset or snapshot restore was
// attempted. It must always be 0.
func (s *Server) ForbiddenCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls["forbidden"]
}

func maps(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *Server) addFault(w http.ResponseWriter, r *http.Request) {
	var f Fault
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&f); err != nil || f.Status < 100 || f.Status > 599 || f.Count < 1 {
		s.writeError(w, http.StatusBadRequest, "body must be a fault with status 100-599 and count >= 1", nil)
		return
	}
	s.Inject(f)
	w.WriteHeader(http.StatusNoContent)
}

// api wraps a handler with call counting, auth, rate limiting and faults.
func (s *Server) api(op string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls[op]++
		s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+Token {
			s.writeError(w, http.StatusUnauthorized, "Unauthenticated.", nil)
			return
		}
		if !s.rateLimit(w) {
			return
		}
		if s.fault(w, r) {
			return
		}
		next(w, r)
	}
}

// rateLimit sets the rate limit headers and answers 429 when the window is
// used up. It returns false if the request must stop.
func (s *Server) rateLimit(w http.ResponseWriter) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	limit := s.limit
	if limit == 0 {
		limit = 90
	} else {
		if s.windowStart.IsZero() || now.Sub(s.windowStart) >= s.window {
			s.windowStart, s.used = now, 0
		}
		s.used++
	}
	remaining := max(0, limit-s.used)
	if s.limit == 0 {
		remaining = limit - 1
	}
	reset := int(s.window.Seconds())
	if !s.windowStart.IsZero() {
		reset = max(1, int((s.window - now.Sub(s.windowStart)).Seconds()))
	}
	h := w.Header()
	h.Set("RateLimit-Policy", fmt.Sprintf(`"api";q=%d;w=%d`, limit, int(s.window.Seconds())))
	h.Set("RateLimit", fmt.Sprintf(`"api";r=%d;t=%d`, remaining, reset))
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	if s.limit > 0 && s.used > s.limit {
		h.Set("Retry-After", strconv.Itoa(reset))
		s.writeErrorLocked(w, http.StatusTooManyRequests, "Too Many Attempts.", nil)
		return false
	}
	return true
}

func (s *Server) fault(w http.ResponseWriter, r *http.Request) bool {
	// Read the body once so faults can match on it, then put it back.
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.faults {
		if f.Count <= 0 {
			continue
		}
		if (f.Method == "" || f.Method == r.Method) && strings.Contains(r.URL.Path, f.Path) &&
			(f.BodyContains == "" || bytes.Contains(body, []byte(f.BodyContains))) {
			f.Count--
			for k, v := range f.Header {
				w.Header()[k] = v
			}
			s.writeErrorLocked(w, f.Status, "injected fault", nil)
			return true
		}
	}
	return false
}

// zoneOr403 returns the zone named in the path, or answers 403 like the
// real API does for a domain that is not in the account.
func (s *Server) zoneOr403(w http.ResponseWriter, r *http.Request) (string, *zone) {
	name := strings.ToLower(r.PathValue("domain"))
	s.mu.Lock()
	z := s.zones[name]
	s.mu.Unlock()
	if z == nil {
		s.writeError(w, http.StatusForbidden, fmt.Sprintf("[DNS:4002] Customer does not own %s domain", name), nil)
		return "", nil
	}
	return name, z
}

func (s *Server) listDomains(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for i, name := range s.order {
		if !s.zones[name].inPortfolio {
			continue
		}
		out = append(out, map[string]any{
			"id": 1000 + i, "domain": name, "type": "domain", "status": s.zones[name].status,
			"created_at": "2026-11-04T00:00:00Z", "expires_at": "2027-11-04T00:00:00Z",
		})
	}
	// An unclaimed free domain has no name, like on the real API.
	out = append(out, map[string]any{"id": 999, "domain": nil, "type": "free_domain", "status": "pending_setup"})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getDomain(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(r.PathValue("domain"))
	s.mu.Lock()
	z := s.zones[name]
	s.mu.Unlock()
	if z == nil || !z.inPortfolio {
		s.writeError(w, http.StatusNotFound, "[Domains:2006] Domain is not registered at Hostinger.", nil)
		return
	}
	ns := map[string]string{}
	for i, n := range z.nameServers {
		ns["ns"+strconv.Itoa(i+1)] = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"domain": name, "status": "Active", "name_servers": ns})
}

func (s *Server) getZone(w http.ResponseWriter, r *http.Request) {
	name, z := s.zoneOr403(w, r)
	if z == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.RRsets(name))
}

type updateBody struct {
	Overwrite *bool        `json:"overwrite"`
	Zone      []updateItem `json:"zone"`
}

type updateItem struct {
	Name    *string `json:"name"`
	Type    string  `json:"type"`
	TTL     *int64  `json:"ttl"`
	Records []struct {
		Content *string `json:"content"`
	} `json:"records"`
}

func (s *Server) updateZone(w http.ResponseWriter, r *http.Request)   { s.write(w, r, true) }
func (s *Server) validateZone(w http.ResponseWriter, r *http.Request) { s.write(w, r, false) }

// write handles PUT (commit=true) and validate (commit=false).
func (s *Server) write(w http.ResponseWriter, r *http.Request, commit bool) {
	name, z := s.zoneOr403(w, r)
	if z == nil {
		return
	}
	var body updateBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body); err != nil {
		s.writeError(w, http.StatusUnprocessableEntity, "The zone field is required.", map[string][]string{"zone": {"The zone field is required."}})
		return
	}
	if fields := checkUpdateFields(body); len(fields) > 0 {
		s.writeFieldErrors(w, fields)
		return
	}
	overwrite := body.Overwrite == nil || *body.Overwrite

	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneSets(z.sets)
	for _, it := range body.Zone {
		rel, err := relativeName(*it.Name, name)
		if err != nil {
			s.writeErrorLocked(w, http.StatusUnprocessableEntity, "[DNS:4005] DNS record validation error : "+err.Error(), nil)
			return
		}
		ttl := int64(1800)
		if it.TTL != nil {
			ttl = *it.TTL
		}
		if ttl < 60 {
			s.writeErrorLocked(w, http.StatusUnprocessableEntity, "[DNS:4005] Resource record TTL must be at least 60 seconds!", nil)
			return
		}
		if ttl > 86400 {
			s.writeErrorLocked(w, http.StatusUnprocessableEntity, "[DNS:4005] Resource record TTL cannot exceed 86400 seconds!", nil)
			return
		}
		// The real API refuses SOA everywhere and NS below the apex. NS at
		// the apex passes /validate; nobody has tried a real write, because
		// it would change the domain's delegation.
		if it.Type == "SOA" || (it.Type == "NS" && rel != "@") {
			s.writeErrorLocked(w, http.StatusUnprocessableEntity, "[DNS:4005] Resource record type is invalid, unsupported or immutable! "+
				"Supported types: A, AAAA, ALIAS, CNAME, MX, TXT, SRV, CAA.", nil)
			return
		}
		fqdn := fqdnOf(rel, name)
		var values []Record
		for _, rec := range it.Records {
			c, msg := normalizeContent(it.Type, *rec.Content)
			if msg != "" {
				s.writeErrorLocked(w, http.StatusUnprocessableEntity, msg, nil)
				return
			}
			if slices.ContainsFunc(values, func(v Record) bool { return v.Content == c }) {
				s.writeErrorLocked(w, http.StatusUnprocessableEntity, fmt.Sprintf(
					"[DNS:4005] DNS record validation error : Duplicate record in RRset %s IN %s with content %s", fqdn, it.Type, c), nil)
				return
			}
			values = append(values, Record{Content: c})
		}
		i := findSet(next, rel, it.Type)
		switch {
		case i < 0:
			next = append(next, &RRset{Name: rel, Type: it.Type, TTL: ttl, Records: values})
		case overwrite:
			next[i] = &RRset{Name: rel, Type: it.Type, TTL: ttl, Records: values}
		default:
			for _, v := range values {
				if slices.ContainsFunc(next[i].Records, func(e Record) bool { return e.Content == v.Content }) {
					s.writeErrorLocked(w, http.StatusUnprocessableEntity, "[DNS:4008] DNS resource record is not valid or conflicts with another resource record", nil)
					return
				}
			}
			next[i].Records = append(next[i].Records, values...)
			next[i].TTL = ttl
		}
	}
	if msg := checkZone(next, name); msg != "" {
		s.writeErrorLocked(w, http.StatusUnprocessableEntity, msg, nil)
		return
	}
	if commit {
		z.sets = next
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Request accepted"})
}

func checkUpdateFields(b updateBody) map[string][]string {
	fields := map[string][]string{}
	if len(b.Zone) == 0 {
		fields["zone"] = []string{"The zone field is required."}
	}
	for i, it := range b.Zone {
		p := "zone." + strconv.Itoa(i)
		if it.Name == nil || *it.Name == "" {
			fields[p+".name"] = []string{"The " + p + ".name field is required."}
		}
		if it.Type == "" {
			fields[p+".type"] = []string{"The " + p + ".type field is required."}
		} else if !validTypes[it.Type] {
			fields[p+".type"] = []string{"The selected " + p + ".type is invalid."}
		}
		if len(it.Records) == 0 {
			fields[p+".records"] = []string{"The " + p + ".records field is required."}
		}
		for j, rec := range it.Records {
			if rec.Content == nil || *rec.Content == "" {
				k := p + ".records." + strconv.Itoa(j) + ".content"
				fields[k] = []string{"The " + k + " field is required."}
			}
		}
	}
	return fields
}

// checkZone enforces the rules that involve several RRsets.
func checkZone(sets []*RRset, zoneName string) string {
	for _, set := range sets {
		if set.Type != "CNAME" {
			continue
		}
		fqdn := fqdnOf(set.Name, zoneName)
		if len(set.Records) > 1 {
			return fmt.Sprintf("[DNS:4005] DNS record validation error : RRset %s IN CNAME has more than one record", fqdn)
		}
		for _, other := range sets {
			if other != set && other.Name == set.Name {
				return fmt.Sprintf("[DNS:4005] DNS record validation error : RRset %s IN CNAME must not be used with any other type on the same name.", fqdn)
			}
		}
	}
	return ""
}

type deleteBody struct {
	Filters []struct {
		Name *string `json:"name"`
		Type string  `json:"type"`
	} `json:"filters"`
}

func (s *Server) deleteRRsets(w http.ResponseWriter, r *http.Request) {
	_, z := s.zoneOr403(w, r)
	if z == nil {
		return
	}
	var body deleteBody
	_ = json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body)
	fields := map[string][]string{}
	if len(body.Filters) == 0 {
		fields["filters"] = []string{"The filters field is required."}
	}
	for i, f := range body.Filters {
		p := "filters." + strconv.Itoa(i)
		if f.Name == nil || *f.Name == "" {
			fields[p+".name"] = []string{"The " + p + ".name field is required."}
		}
		if !validTypes[f.Type] {
			fields[p+".type"] = []string{"The selected " + p + ".type is invalid."}
		}
	}
	if len(fields) > 0 {
		s.writeFieldErrors(w, fields)
		return
	}
	s.mu.Lock()
	for _, f := range body.Filters {
		// The real API matches the stored name exactly. "WWW" or
		// "www.example.com" match nothing, and the answer is still 200.
		if i := z.find(*f.Name, f.Type); i >= 0 {
			z.sets = slices.Delete(z.sets, i, i+1)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"message": "Request accepted"})
}

func (z *zone) find(name, typ string) int { return findSet(z.sets, name, typ) }

func findSet(sets []*RRset, name, typ string) int {
	return slices.IndexFunc(sets, func(r *RRset) bool { return r.Name == name && r.Type == typ })
}

func cloneSets(in []*RRset) []*RRset {
	out := make([]*RRset, 0, len(in))
	for _, r := range in {
		c := *r
		c.Records = slices.Clone(r.Records)
		out = append(out, &c)
	}
	return out
}

// relativeName turns a name from a PUT body into the stored form.
func relativeName(name, zoneName string) (string, error) {
	n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	switch {
	case n == "@" || n == zoneName:
		return "@", nil
	case strings.HasSuffix(n, "."+zoneName):
		n = strings.TrimSuffix(n, "."+zoneName)
	}
	for i, label := range strings.Split(n, ".") {
		if label == "*" && i == 0 {
			continue
		}
		if label == "" || len(label) > 63 || strings.ContainsFunc(label, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_'
		}) {
			return "", fmt.Errorf("Invalid RRset name %s", fqdnOf(n, zoneName)) //nolint:staticcheck // copies the real message
		}
	}
	return n, nil
}

func fqdnOf(rel, zoneName string) string {
	if rel == "@" {
		return zoneName
	}
	return rel + "." + zoneName
}

// normalizeContent checks a value and rewrites it the way the real API
// stores it. It returns an error message for invalid content.
func normalizeContent(typ, c string) (string, string) {
	c = strings.TrimSpace(c)
	invalid := "[DNS:4008] DNS resource record is not valid or conflicts with another resource record"
	switch typ {
	case "A":
		ip, err := netip.ParseAddr(c)
		if err != nil || !ip.Is4() {
			return "", `[DNS:4005] "A" record content must be a valid IPv4 address!`
		}
		return ip.String(), ""
	case "AAAA":
		ip, err := netip.ParseAddr(c)
		if err != nil || !ip.Is6() || ip.Is4In6() {
			return "", `[DNS:4005] "AAAA" record content must be a valid IPv6 address!`
		}
		return ip.String(), ""
	case "CNAME", "ALIAS", "NS":
		if !hostname(c) {
			return "", invalid
		}
		return withDot(c), ""
	case "MX":
		f := strings.Fields(c)
		if len(f) != 2 || !uint16Str(f[0]) || !hostname(f[1]) {
			return "", invalid
		}
		return f[0] + " " + withDot(f[1]), ""
	case "SRV":
		f := strings.Fields(c)
		if len(f) != 4 || !uint16Str(f[0]) || !uint16Str(f[1]) || !uint16Str(f[2]) || !hostname(f[3]) {
			return "", invalid
		}
		return strings.Join(f[:3], " ") + " " + withDot(f[3]), ""
	case "CAA":
		parts := strings.SplitN(c, " ", 3)
		if len(parts) != 3 {
			return "", invalid
		}
		flag, err := strconv.Atoi(parts[0])
		if err != nil || flag < 0 || flag > 255 || parts[1] == "" {
			return "", invalid
		}
		v := parts[2]
		if !quoted(v) {
			v = `"` + v + `"`
		}
		return parts[0] + " " + parts[1] + " " + v, ""
	case "TXT":
		if !quoted(c) {
			c = `"` + c + `"`
		}
		if !validTXT(c) {
			return "", invalid
		}
		// The real API stores "\\" as "\".
		return strings.ReplaceAll(c, `\\`, `\`), ""
	}
	return c, ""
}

func quoted(s string) bool { return len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' }

// validTXT accepts one or more quoted strings separated by spaces, where a
// quote inside a string is escaped with a backslash.
func validTXT(c string) bool {
	in := false
	for i := 0; i < len(c); i++ {
		switch ch := c[i]; {
		case ch == '\\' && in:
			i++
		case ch == '"':
			in = !in
		case !in && ch != ' ':
			return false
		}
	}
	return !in
}

func hostname(s string) bool {
	s = strings.TrimSuffix(s, ".")
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
	}
	return !strings.ContainsAny(s, " \"@")
}

func withDot(s string) string {
	if strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}

func uint16Str(s string) bool {
	_, err := strconv.ParseUint(s, 10, 16)
	return err == nil
}

func (s *Server) writeFieldErrors(w http.ResponseWriter, fields map[string][]string) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	msg := fields[keys[0]][0]
	if len(keys) > 1 {
		msg += fmt.Sprintf(" (and %d more error", len(keys)-1)
		if len(keys) > 2 {
			msg += "s"
		}
		msg += ")"
	}
	s.writeError(w, http.StatusUnprocessableEntity, msg, fields)
}

func (s *Server) writeError(w http.ResponseWriter, code int, msg string, fields map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeErrorLocked(w, code, msg, fields)
}

func (s *Server) writeErrorLocked(w http.ResponseWriter, code int, msg string, fields map[string][]string) {
	s.corrSeq++
	body := map[string]any{"message": msg, "correlation_id": fmt.Sprintf("fake-%08d", s.corrSeq)}
	if fields != nil {
		body["errors"] = fields
	}
	writeJSON(w, code, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
