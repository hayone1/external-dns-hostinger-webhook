package hostingertest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// These tests pin the fake to what the real API did when it was probed on
// 2026-11-04 (docs/hostinger-api.md). If the real API changes, change the
// doc, the fake and these tests together.

const zoneName = "example.com"

func call(t *testing.T, s *Server, method, path, body string) (int, string, http.Header) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func put(t *testing.T, s *Server, body string) (int, string) {
	t.Helper()
	code, b, _ := call(t, s, http.MethodPut, "/api/dns/v1/zones/"+zoneName, body)
	return code, b
}

func set(t *testing.T, s *Server, name, typ string) *RRset {
	t.Helper()
	for _, r := range s.RRsets(zoneName) {
		if r.Name == name && r.Type == typ {
			return &r
		}
	}
	return nil
}

func newFake(t *testing.T) *Server {
	t.Helper()
	s := New()
	t.Cleanup(s.Close)
	s.AddZone(zoneName)
	return s
}

func TestDefaultRecords(t *testing.T) {
	s := newFake(t)
	code, body, _ := call(t, s, http.MethodGet, "/api/dns/v1/zones/"+zoneName, "")
	if code != 200 {
		t.Fatalf("GET = %d %s", code, body)
	}
	var sets []RRset
	if err := json.Unmarshal([]byte(body), &sets); err != nil {
		t.Fatal(err)
	}
	if len(sets) != 2 {
		t.Fatalf("a fresh zone has an apex A and a www CNAME, got %+v", sets)
	}
	if !strings.Contains(body, `"is_disabled":false`) {
		t.Errorf("values must carry is_disabled: %s", body)
	}
	for _, r := range sets {
		if r.Type == "NS" || r.Type == "SOA" {
			t.Errorf("NS and SOA are never listed: %+v", r)
		}
	}
}

func TestNames(t *testing.T) {
	s := newFake(t)
	for _, n := range []string{"full.example.com", "dot.example.com.", "UPPER", "*.wild", "_under", "example.com"} {
		if code, b := put(t, s, `{"overwrite":true,"zone":[{"name":"`+n+`","type":"TXT","ttl":300,"records":[{"content":"x"}]}]}`); code != 200 {
			t.Fatalf("name %q: %d %s", n, code, b)
		}
	}
	for _, want := range []string{"full", "dot", "upper", "*.wild", "_under", "@"} {
		if set(t, s, want, "TXT") == nil {
			t.Errorf("name not stored as %q: %+v", want, s.RRsets(zoneName))
		}
	}
	code, b := put(t, s, `{"zone":[{"name":"a.*.mid","type":"A","ttl":300,"records":[{"content":"1.1.1.1"}]}]}`)
	if code != 422 || !strings.Contains(b, "[DNS:4005] DNS record validation error : Invalid RRset name a.*.mid.example.com") {
		t.Errorf("a * in the middle must be refused: %d %s", code, b)
	}
	code, b = put(t, s, `{"zone":[{"name":"","type":"A","ttl":300,"records":[{"content":"1.1.1.1"}]}]}`)
	if code != 422 || !strings.Contains(b, `"zone.0.name":["The zone.0.name field is required."]`) {
		t.Errorf("an empty name must be a field error: %d %s", code, b)
	}
}

func TestTTL(t *testing.T) {
	s := newFake(t)
	for ttl, want := range map[string]string{"59": "at least 60 seconds", "86401": "cannot exceed 86400 seconds"} {
		code, b := put(t, s, `{"zone":[{"name":"t","type":"A","ttl":`+ttl+`,"records":[{"content":"1.1.1.1"}]}]}`)
		if code != 422 || !strings.Contains(b, want) {
			t.Errorf("ttl %s: %d %s", ttl, code, b)
		}
	}
	for _, ttl := range []string{"60", "86400"} {
		if code, b := put(t, s, `{"zone":[{"name":"t","type":"A","ttl":`+ttl+`,"records":[{"content":"1.1.1.1"}]}]}`); code != 200 {
			t.Errorf("ttl %s must be accepted: %d %s", ttl, code, b)
		}
	}
	put(t, s, `{"zone":[{"name":"nottl","type":"A","records":[{"content":"1.1.1.1"}]}]}`)
	if r := set(t, s, "nottl", "A"); r == nil || r.TTL != 1800 {
		t.Errorf("a missing TTL becomes 1800: %+v", r)
	}
}

func TestContentIsRewritten(t *testing.T) {
	s := newFake(t)
	body := `{"zone":[
		{"name":"txt","type":"TXT","ttl":300,"records":[{"content":"plain"},{"content":"\"quoted\""},{"content":"\"a\" \"b\""}]},
		{"name":"cname","type":"CNAME","ttl":300,"records":[{"content":"Target.Example.net"}]},
		{"name":"mx","type":"MX","ttl":300,"records":[{"content":"10 mail.example.com"}]},
		{"name":"_sip._tcp","type":"SRV","ttl":300,"records":[{"content":"10 5 5060 sip.example.com"}]},
		{"name":"caa","type":"CAA","ttl":300,"records":[{"content":"0 iodef mailto:a@example.com"}]},
		{"name":"v6","type":"AAAA","ttl":300,"records":[{"content":"2606:4700:4700:0:0:0:0:1111"}]},
		{"name":"private","type":"A","ttl":300,"records":[{"content":"10.0.0.5"}]}
	]}`
	if code, b := put(t, s, body); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	want := map[string]string{
		"txt TXT":       `"plain" "quoted" "a" "b"`,
		"cname CNAME":   "Target.Example.net.",
		"mx MX":         "10 mail.example.com.",
		"_sip._tcp SRV": "10 5 5060 sip.example.com.",
		"caa CAA":       `0 iodef "mailto:a@example.com"`,
		"v6 AAAA":       "2606:4700:4700::1111",
		"private A":     "10.0.0.5",
	}
	for k, v := range want {
		f := strings.Fields(k)
		r := set(t, s, f[0], f[1])
		if r == nil || strings.Join(r.Values(), " ") != v {
			t.Errorf("%s stored as %+v, want %s", k, r, v)
		}
	}
	if code, b := put(t, s, `{"zone":[{"name":"bad","type":"TXT","ttl":300,"records":[{"content":"say \"hi\""}]}]}`); code != 422 || !strings.Contains(b, "DNS:4008") {
		t.Errorf("a bare quote inside TXT must be refused: %d %s", code, b)
	}
}

func TestRRsetRules(t *testing.T) {
	s := newFake(t)
	cases := map[string]string{
		`{"zone":[{"name":"d","type":"A","ttl":300,"records":[{"content":"1.1.1.1"},{"content":"1.1.1.1"}]}]}`:                 "Duplicate record in RRset d.example.com IN A with content 1.1.1.1",
		`{"zone":[{"name":"c","type":"CNAME","ttl":300,"records":[{"content":"a.example.net"},{"content":"b.example.net"}]}]}`: "has more than one record",
		`{"zone":[{"name":"www","type":"TXT","ttl":300,"records":[{"content":"x"}]}]}`:                                         "must not be used with any other type on the same name",
		`{"zone":[{"name":"e","type":"A","ttl":300,"records":[]}]}`:                                                            "The zone.0.records field is required.",
		`{"zone":[{"name":"x","type":"A","ttl":300,"records":[{"content":"not-an-ip"}]}]}`:                                     `\"A\" record content must be a valid IPv4 address!`,
		`{"zone":[{"name":"x","type":"a","ttl":300,"records":[{"content":"1.1.1.1"}]}]}`:                                       "The selected zone.0.type is invalid.",
	}
	for body, want := range cases {
		if code, b := put(t, s, body); code != 422 || !strings.Contains(b, want) {
			t.Errorf("%s\n got %d %s\nwant 422 with %q", body, code, b, want)
		}
	}
}

func TestBatchIsAllOrNothing(t *testing.T) {
	s := newFake(t)
	code, _ := put(t, s, `{"zone":[{"name":"good","type":"A","ttl":300,"records":[{"content":"1.1.1.1"}]},{"name":"bad","type":"A","ttl":30,"records":[{"content":"1.1.1.1"}]}]}`)
	if code != 422 {
		t.Fatalf("code = %d", code)
	}
	if set(t, s, "good", "A") != nil {
		t.Fatal("the good RRset of a refused batch must not be written")
	}
}

func TestOverwrite(t *testing.T) {
	s := newFake(t)
	put(t, s, `{"overwrite":true,"zone":[{"name":"o","type":"A","ttl":300,"records":[{"content":"1.1.1.1"},{"content":"1.0.0.1"}]}]}`)
	put(t, s, `{"overwrite":false,"zone":[{"name":"o","type":"A","ttl":600,"records":[{"content":"8.8.8.8"}]}]}`)
	if r := set(t, s, "o", "A"); r == nil || len(r.Records) != 3 || r.TTL != 600 {
		t.Fatalf("overwrite=false appends and updates the TTL: %+v", r)
	}
	if code, b := put(t, s, `{"overwrite":false,"zone":[{"name":"o","type":"A","ttl":600,"records":[{"content":"8.8.8.8"}]}]}`); code != 422 || !strings.Contains(b, "DNS:4008") {
		t.Errorf("appending an existing value is 422 [DNS:4008]: %d %s", code, b)
	}
	put(t, s, `{"zone":[{"name":"o","type":"A","ttl":300,"records":[{"content":"9.9.9.9"}]}]}`)
	if r := set(t, s, "o", "A"); r == nil || strings.Join(r.Values(), ",") != "9.9.9.9" {
		t.Fatalf("overwrite defaults to true and replaces the RRset: %+v", r)
	}
}

func TestDeleteMatchesTheStoredNameOnly(t *testing.T) {
	s := newFake(t)
	put(t, s, `{"zone":[{"name":"keep","type":"A","ttl":300,"records":[{"content":"1.1.1.1"}]}]}`)
	for _, n := range []string{"keep.example.com", "KEEP", "keep.example.com.", "missing"} {
		code, b, _ := call(t, s, http.MethodDelete, "/api/dns/v1/zones/"+zoneName, `{"filters":[{"name":"`+n+`","type":"A"}]}`)
		if code != 200 {
			t.Fatalf("%s: %d %s", n, code, b)
		}
	}
	if set(t, s, "keep", "A") == nil {
		t.Fatal("only the exact stored name deletes an RRset")
	}
	call(t, s, http.MethodDelete, "/api/dns/v1/zones/"+zoneName, `{"filters":[{"name":"keep","type":"A"},{"name":"@","type":"A"}]}`)
	if set(t, s, "keep", "A") != nil || set(t, s, "@", "A") != nil {
		t.Fatal("exact names must delete")
	}
	if code, b, _ := call(t, s, http.MethodDelete, "/api/dns/v1/zones/"+zoneName, `{"filters":[]}`); code != 422 || !strings.Contains(b, "The filters field is required.") {
		t.Errorf("empty filters: %d %s", code, b)
	}
	if code, b, _ := call(t, s, http.MethodDelete, "/api/dns/v1/zones/"+zoneName, `{"filters":[{"name":"x","type":"a"}]}`); code != 422 || !strings.Contains(b, "filters.0.type is invalid") {
		t.Errorf("lower case type: %d %s", code, b)
	}
}

func TestValidateDoesNotWrite(t *testing.T) {
	s := newFake(t)
	code, _, _ := call(t, s, http.MethodPost, "/api/dns/v1/zones/"+zoneName+"/validate", `{"zone":[{"name":"v","type":"A","ttl":300,"records":[{"content":"1.1.1.1"}]}]}`)
	if code != 200 || set(t, s, "v", "A") != nil {
		t.Fatalf("validate = %d and must not write", code)
	}
	code, _, _ = call(t, s, http.MethodPost, "/api/dns/v1/zones/"+zoneName+"/validate", `{"zone":[{"name":"v","type":"A","ttl":1,"records":[{"content":"1.1.1.1"}]}]}`)
	if code != 422 {
		t.Fatalf("validate of a bad RRset = %d", code)
	}
}

func TestAccountAndAuth(t *testing.T) {
	s := newFake(t)
	s.AddForeignZone("elsewhere.net")
	code, b, _ := call(t, s, http.MethodGet, "/api/dns/v1/zones/other.org", "")
	if code != 403 || !strings.Contains(b, "[DNS:4002] Customer does not own other.org domain") || !strings.Contains(b, "correlation_id") {
		t.Errorf("unknown zone: %d %s", code, b)
	}
	code, b, _ = call(t, s, http.MethodGet, "/api/domains/v1/portfolio", "")
	if code != 200 || !strings.Contains(b, `"domain":"example.com"`) || strings.Contains(b, "elsewhere.net") || !strings.Contains(b, `"domain":null`) {
		t.Errorf("portfolio: %d %s", code, b)
	}
	if code, _, _ := call(t, s, http.MethodGet, "/api/dns/v1/zones/elsewhere.net", ""); code != 200 {
		t.Errorf("a zone outside the portfolio can still be read: %d", code)
	}
	code, b, _ = call(t, s, http.MethodGet, "/api/domains/v1/portfolio/example.com", "")
	if code != 200 || !strings.Contains(b, "ns1.dns-parking.com") {
		t.Errorf("domain details: %d %s", code, b)
	}

	req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/dns/v1/zones/"+zoneName, nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(b2), `"message":"Unauthenticated."`) {
		t.Errorf("bad token: %d %s", resp.StatusCode, b2)
	}
}

func TestForbiddenCallsAreCounted(t *testing.T) {
	s := newFake(t)
	call(t, s, http.MethodPost, "/api/dns/v1/zones/"+zoneName+"/reset", `{}`)
	call(t, s, http.MethodPost, "/api/dns/v1/snapshots/"+zoneName+"/1/restore", ``)
	if s.ForbiddenCalls() != 2 {
		t.Fatalf("forbidden calls = %d", s.ForbiddenCalls())
	}
	if len(s.RRsets(zoneName)) != 2 {
		t.Fatal("a reset must change nothing in the fake")
	}
}

func TestRateLimitAndFaults(t *testing.T) {
	s := newFake(t)
	_, _, h := call(t, s, http.MethodGet, "/api/dns/v1/zones/"+zoneName, "")
	if h.Get("RateLimit-Policy") != `"api";q=90;w=60` || !strings.HasPrefix(h.Get("RateLimit"), `"api";r=89;t=`) {
		t.Errorf("rate limit headers: %v", h)
	}
	s.SetRateLimit(2, time.Minute)
	call(t, s, http.MethodGet, "/api/dns/v1/zones/"+zoneName, "")
	call(t, s, http.MethodGet, "/api/dns/v1/zones/"+zoneName, "")
	code, _, h := call(t, s, http.MethodGet, "/api/dns/v1/zones/"+zoneName, "")
	if code != 429 || h.Get("Retry-After") == "" {
		t.Errorf("third call in the window = %d, Retry-After %q", code, h.Get("Retry-After"))
	}
	s.SetRateLimit(0, time.Minute)

	s.Inject(Fault{Method: "PUT", Status: 503, Count: 1})
	if code, _ := put(t, s, `{"zone":[{"name":"f","type":"A","ttl":300,"records":[{"content":"1.1.1.1"}]}]}`); code != 503 {
		t.Errorf("fault not applied: %d", code)
	}
	if code, _ := put(t, s, `{"zone":[{"name":"f","type":"A","ttl":300,"records":[{"content":"1.1.1.1"}]}]}`); code != 200 {
		t.Errorf("fault must be used up: %d", code)
	}
	if s.Calls("updateZone") != 2 || s.MutatingCalls() != 2 {
		t.Errorf("calls = %d / %d", s.Calls("updateZone"), s.MutatingCalls())
	}
}
