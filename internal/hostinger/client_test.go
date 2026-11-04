package hostinger

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hayone1/external-dns-hostinger-webhook/internal/hostinger/hostingertest"
)

func newTestClient(t *testing.T, url, token string) *Client {
	t.Helper()
	c, err := New(Options{APIURL: url, Token: token, RateLimit: 1000, Burst: 100, MaxRetries: 3, BaseBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func fake(t *testing.T) (*hostingertest.Server, *Client) {
	t.Helper()
	s := hostingertest.New()
	t.Cleanup(s.Close)
	s.AddZone("example.com")
	return s, newTestClient(t, s.URL, hostingertest.Token)
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{Token: "x", APIURL: "not a url"}); err == nil {
		t.Error("bad URL must fail")
	}
	if _, err := New(Options{APIURL: DefaultAPIURL}); err == nil {
		t.Error("missing token must fail")
	}
	c, err := New(Options{Token: "x"})
	if err != nil || c.base != DefaultAPIURL {
		t.Errorf("default URL: %v %v", c, err)
	}
}

func TestRoundTrip(t *testing.T) {
	s, c := fake(t)
	ctx := context.Background()

	domains, err := c.ListDomains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) != 2 || domains[0].Domain == nil || *domains[0].Domain != "example.com" || domains[1].Domain != nil {
		t.Fatalf("domains = %+v", domains)
	}
	d, err := c.GetDomain(ctx, "example.com")
	if err != nil || d.NameServers["ns1"] != "ns1.dns-parking.com" {
		t.Fatalf("domain = %+v, %v", d, err)
	}

	sets := []RRset{
		{Name: "www2", Type: TypeA, TTL: 300, Records: []Record{{Content: "1.1.1.1"}, {Content: "1.0.0.1"}}},
		{Name: "txt", Type: TypeTXT, TTL: 300, Records: []Record{{Content: `"hello"`}}},
	}
	if err := c.ValidateZone(ctx, "example.com", sets); err != nil {
		t.Fatal(err)
	}
	if s.MutatingCalls() != 0 {
		t.Fatal("validate must not write")
	}
	if err := c.UpdateZone(ctx, "example.com", sets); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetZone(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("zone = %+v", got)
	}
	if err := c.DeleteRRsets(ctx, "example.com", []Filter{{Name: "www2", Type: TypeA}, {Name: "missing", Type: TypeA}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetZone(ctx, "example.com"); len(got) != 3 {
		t.Fatalf("delete did not work: %+v", got)
	}
	if err := c.DeleteRRsets(ctx, "example.com", nil); err != nil || s.Calls("deleteRRsets") != 1 {
		t.Fatalf("an empty delete must not call the API: %v", err)
	}
}

func TestProtectedTypesAreRefused(t *testing.T) {
	s, c := fake(t)
	ctx := context.Background()
	for _, typ := range []string{TypeNS, TypeSOA} {
		if err := c.UpdateZone(ctx, "example.com", []RRset{{Name: "@", Type: typ, TTL: 300, Records: []Record{{Content: "x."}}}}); err == nil {
			t.Errorf("writing %s must be refused", typ)
		}
		if err := c.DeleteRRsets(ctx, "example.com", []Filter{{Name: "@", Type: typ}}); err == nil {
			t.Errorf("deleting %s must be refused", typ)
		}
	}
	if err := c.UpdateZone(ctx, "example.com", []RRset{{Name: "x", Type: TypeA, TTL: 300}}); err == nil {
		t.Error("an RRset without values must be refused locally (Hostinger answers 422)")
	}
	if err := c.UpdateZone(ctx, "example.com", nil); err == nil {
		t.Error("an empty write must be refused")
	}
	if s.MutatingCalls() != 0 {
		t.Fatal("refused writes must not reach the API")
	}
}

func TestErrorsAreTyped(t *testing.T) {
	s, c := fake(t)
	ctx := context.Background()

	_, err := c.GetZone(ctx, "other.org")
	if !IsNotOwned(err) || IsTemporary(err) {
		t.Fatalf("unknown zone: %v", err)
	}
	if !strings.Contains(err.Error(), "correlation_id fake-") || !strings.Contains(err.Error(), "Customer does not own other.org domain") {
		t.Errorf("error text: %v", err)
	}
	apiErr, _ := errors.AsType[*APIError](err)
	if apiErr.Code != "DNS:4002" {
		t.Errorf("code = %q", apiErr.Code)
	}

	err = c.UpdateZone(ctx, "example.com", []RRset{{Name: "x", Type: TypeA, TTL: 1, Records: []Record{{Content: "1.1.1.1"}}}})
	if !IsValidation(err) || IsTemporary(err) {
		t.Fatalf("422: %v", err)
	}

	bad := newTestClient(t, s.URL, "wrong-token-value")
	err = bad.CheckToken(ctx)
	if !errors.Is(err, ErrInvalidToken) || IsTemporary(err) {
		t.Fatalf("401: %v", err)
	}
	if strings.Contains(err.Error(), "wrong-token-value") {
		t.Fatal("the token must never show up in an error")
	}
	if s.Calls("listDomains") != 1 {
		t.Errorf("a 401 must not be retried: %d calls", s.Calls("listDomains"))
	}
}

func TestFieldErrors(t *testing.T) {
	e := parseError("updateZone", 422, []byte(`{"message":"The zone.0.name field is required. (and 1 more error)","errors":{"zone.0.name":["The zone.0.name field is required."],"zone.0.ttl":["The zone.0.ttl must be an integer."]},"correlation_id":"abc"}`))
	msg := e.Error()
	for _, want := range []string{"HTTP 422", "zone.0.name: The zone.0.name field is required.", "zone.0.ttl:", "(correlation_id abc)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q does not contain %q", msg, want)
		}
	}
	e = parseError("getZone", 502, []byte("<html>"+strings.Repeat("x", 2000)+"</html>"))
	if len(e.Message) > 520 || !e.Temporary() {
		t.Errorf("HTML error page: %d bytes, temporary %v", len(e.Message), e.Temporary())
	}
}

func TestRetries(t *testing.T) {
	s, c := fake(t)
	ctx := context.Background()

	s.Inject(hostingertest.Fault{Method: "GET", Status: 503, Count: 2})
	if _, err := c.GetZone(ctx, "example.com"); err != nil {
		t.Fatalf("503 must be retried: %v", err)
	}
	if s.Calls("getZone") != 3 {
		t.Errorf("calls = %d, want 3", s.Calls("getZone"))
	}

	s.Inject(hostingertest.Fault{Method: "PUT", Status: 500, Count: 10})
	err := c.UpdateZone(ctx, "example.com", []RRset{{Name: "x", Type: TypeA, TTL: 300, Records: []Record{{Content: "1.1.1.1"}}}})
	if !IsTemporary(err) {
		t.Fatalf("after the retries the error stays temporary: %v", err)
	}
	if s.Calls("updateZone") != 4 {
		t.Errorf("1 try + 3 retries expected, got %d", s.Calls("updateZone"))
	}
	s.ClearFaults()

	// 429 with Retry-After: the client waits at least that long.
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	s.Inject(hostingertest.Fault{Status: 429, Count: 1, Header: http.Header{"Retry-After": {"7"}}})
	if _, err := c.GetZone(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("slept %v, want [7s]", slept)
	}
}

func TestPausesWhenTheBudgetIsUsedUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("RateLimit", `"api";r=0;t=12`)
		} else {
			w.Header().Set("RateLimit", `"api";r=50;t=40`)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, "t")
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		now = now.Add(d)
		return nil
	}

	ctx := context.Background()
	if _, err := c.GetZone(ctx, "a.com"); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 0 {
		t.Fatalf("no pause before the first call: %v", slept)
	}
	if _, err := c.GetZone(ctx, "a.com"); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 12*time.Second {
		t.Fatalf("expected one 12s pause after r=0, got %v", slept)
	}
	if _, err := c.GetZone(ctx, "a.com"); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 {
		t.Fatalf("no pause once budget is back: %v", slept)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	if retryAfter(h, now) != 0 {
		t.Error("no header")
	}
	h.Set("Retry-After", "30")
	if retryAfter(h, now) != 30*time.Second {
		t.Error("seconds")
	}
	h.Set("Retry-After", now.Add(10*time.Second).Format(http.TimeFormat))
	if retryAfter(h, now) != 10*time.Second {
		t.Errorf("date: %v", retryAfter(h, now))
	}
	c := newTestClient(t, DefaultAPIURL, "t")
	if d := c.backoff(0, 5*time.Minute); d != maxBackoff {
		t.Errorf("hint must be capped: %v", d)
	}
}

func TestHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"message":"Request accepted"}`))
	}))
	defer srv.Close()
	c, err := New(Options{APIURL: srv.URL, Token: " tok \n", UserAgent: "ua/1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateZone(context.Background(), "a.com", []RRset{{Name: "x", Type: TypeA, TTL: 300, Records: []Record{{Content: "1.1.1.1"}}}}); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer tok" || got.Get("Accept") != "application/json" ||
		got.Get("Content-Type") != "application/json" || got.Get("User-Agent") != "ua/1" {
		t.Errorf("headers = %v", got)
	}
}
