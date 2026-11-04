// Package integration runs real external-dns code against the webhook.
//
// The chain is the same as in a cluster, minus Kubernetes:
//
//	external-dns planner -> TXT registry -> webhook HTTP client
//	  -> our webhook server -> our provider -> our Hostinger client
//	  -> fake Hostinger API
//
// Each scenario runs the reconcile loop twice. The second run must plan no
// changes; otherwise external-dns would fight itself forever in production.
package integration

import (
	"context"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider/webhook"
	"sigs.k8s.io/external-dns/registry"
	"sigs.k8s.io/external-dns/registry/txt"

	"github.com/hayone1/external-dns-hostinger-webhook/internal/hostinger"
	"github.com/hayone1/external-dns-hostinger-webhook/internal/hostinger/hostingertest"
	"github.com/hayone1/external-dns-hostinger-webhook/internal/provider"
	"github.com/hayone1/external-dns-hostinger-webhook/internal/server"
)

const zone = "example.com"

type harness struct {
	t    *testing.T
	api  *hostingertest.Server
	reg  registry.Registry
	pol  plan.Policy
	mgd  []string
	errs int
}

func newHarness(t *testing.T, policy plan.Policy, filter []string, managed ...string) *harness {
	t.Helper()
	api := hostingertest.New()
	t.Cleanup(func() {
		if n := api.ForbiddenCalls(); n != 0 {
			t.Errorf("the webhook called zone reset or snapshot restore %d times", n)
		}
		api.Close()
	})
	api.AddBareZone(zone)
	api.AddBareZone("example.org")
	api.AddZone("fresh.net") // has Hostinger's default records

	client, err := hostinger.New(hostinger.Options{
		APIURL:      api.URL,
		Token:       hostingertest.Token,
		RateLimit:   1000,
		Burst:       100,
		MaxRetries:  2,
		BaseBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := provider.New(client, provider.Options{DomainFilter: endpoint.NewDomainFilter(filter), DefaultTTL: 300})
	hook := httptest.NewServer(server.WebhookHandler(p, nil))
	t.Cleanup(hook.Close)

	if len(managed) == 0 {
		managed = []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME}
	}
	// externaldns.NewConfig returns empty values, so set what the
	// controller would get from its flag defaults.
	cfg := externaldns.NewConfig()
	cfg.WebhookProviderURL = hook.URL
	cfg.WebhookProviderReadTimeout = 5 * time.Second
	cfg.WebhookProviderWriteTimeout = 10 * time.Second
	cfg.TXTOwnerID = "test-cluster"
	// The settings the README recommends. Without a prefix that ends in a
	// dot, the ownership record of an apex name lands outside the zone, and
	// without the wildcard replacement the ownership record of "*.x" has a
	// "*" in the middle of its name, which Hostinger refuses.
	cfg.TXTPrefix = "extdns-%{record_type}."
	cfg.TXTWildcardReplacement = "wildcard"
	cfg.ManagedDNSRecordTypes = managed
	wp, err := webhook.New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := txt.New(cfg, wp)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, api: api, reg: reg, pol: policy, mgd: cfg.ManagedDNSRecordTypes}
}

// run does one reconcile loop and returns the number of planned changes.
func (h *harness) run(desired func() []*endpoint.Endpoint) int {
	h.t.Helper()
	ctx := context.Background()
	current, err := h.reg.Records(ctx)
	if err != nil {
		h.t.Fatalf("Records: %v", err)
	}
	adjusted, err := h.reg.AdjustEndpoints(desired())
	if err != nil {
		h.t.Fatalf("AdjustEndpoints: %v", err)
	}
	p := (&plan.Plan{
		Policies:       []plan.Policy{h.pol},
		Current:        current,
		Desired:        adjusted,
		DomainFilter:   endpoint.MatchAllDomainFilters{h.reg.GetDomainFilter()},
		ManagedRecords: h.mgd,
		OwnerID:        h.reg.OwnerID(),
	}).Calculate()
	c := p.Changes
	n := len(c.Create) + len(c.UpdateNew) + len(c.Delete)
	if n > 0 {
		if err := h.reg.ApplyChanges(ctx, c); err != nil {
			h.errs++
			h.t.Logf("ApplyChanges: %v", err)
		}
	}
	return n
}

// converge runs the loop until it plans nothing, failing if that takes more
// than two runs.
func (h *harness) converge(desired func() []*endpoint.Endpoint) {
	h.t.Helper()
	if n := h.run(desired); n == 0 {
		h.t.Fatal("first run should have planned changes")
	}
	if n := h.run(desired); n != 0 {
		h.t.Fatalf("second run planned %d changes; the webhook is not stable", n)
	}
	if h.errs != 0 {
		h.t.Fatalf("%d ApplyChanges calls failed", h.errs)
	}
}

// dns returns "name type value" lines for every value in a zone.
func (h *harness) dns(z string) []string {
	var out []string
	for _, r := range h.api.RRsets(z) {
		name := z
		if r.Name != "@" {
			name = r.Name + "." + z
		}
		for _, v := range r.Values() {
			out = append(out, name+" "+r.Type+" "+v)
		}
	}
	sort.Strings(out)
	return out
}

func (h *harness) has(z, line string) bool {
	for _, l := range h.dns(z) {
		if l == line {
			return true
		}
	}
	return false
}

func eps(list ...*endpoint.Endpoint) func() []*endpoint.Endpoint {
	return func() []*endpoint.Endpoint {
		out := make([]*endpoint.Endpoint, len(list))
		for i, ep := range list {
			out[i] = ep.DeepCopy()
		}
		return out
	}
}

func TestLifecycle(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil)

	// 1. Create a mix of records.
	v1 := eps(
		endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1", "8.8.8.8"),
		endpoint.NewEndpoint("example.com", "A", "1.0.0.1"),
		endpoint.NewEndpoint("v6.example.com", "AAAA", "2606:4700:4700::1111"),
		endpoint.NewEndpoint("alias.example.com", "CNAME", "www.example.com"),
		endpoint.NewEndpoint("*.apps.example.com", "A", "1.1.1.1"),
		endpoint.NewEndpointWithTTL("short.example.com", "A", 60, "8.8.4.4"),
		endpoint.NewEndpoint("www.example.org", "A", "9.9.9.9"),
	)
	h.converge(v1)
	for _, want := range []string{
		"www.example.com A 1.1.1.1", "www.example.com A 8.8.8.8", "example.com A 1.0.0.1",
		"v6.example.com AAAA 2606:4700:4700::1111", "alias.example.com CNAME www.example.com.",
		"*.apps.example.com A 1.1.1.1", "short.example.com A 8.8.4.4",
	} {
		if !h.has(zone, want) {
			t.Errorf("missing %q in %v", want, h.dns(zone))
		}
	}
	if !h.has("example.org", "www.example.org A 9.9.9.9") {
		t.Errorf("second zone not written: %v", h.dns("example.org"))
	}
	for _, want := range []string{"extdns-a.www.example.com", "extdns-a.example.com", "extdns-a.wildcard.apps.example.com", "extdns-cname.alias.example.com"} {
		found := false
		for _, l := range h.dns(zone) {
			if strings.HasPrefix(l, want+" TXT ") && strings.Contains(l, "external-dns/owner=test-cluster") {
				found = true
			}
		}
		if !found {
			t.Errorf("no ownership TXT record %s: %v", want, h.dns(zone))
		}
	}

	// 2. Change targets and TTLs, swap A for CNAME.
	v2 := eps(
		endpoint.NewEndpoint("www.example.com", "A", "8.8.8.8", "9.9.9.9"),
		endpoint.NewEndpointWithTTL("example.com", "A", 600, "1.0.0.1"),
		endpoint.NewEndpoint("v6.example.com", "AAAA", "2606:4700:4700::1001"),
		endpoint.NewEndpoint("alias.example.com", "CNAME", "other.example.net"),
		endpoint.NewEndpoint("*.apps.example.com", "CNAME", "lb.example.net"),
		endpoint.NewEndpointWithTTL("short.example.com", "A", 60, "8.8.4.4"),
		endpoint.NewEndpoint("www.example.org", "A", "9.9.9.9"),
	)
	h.converge(v2)
	if h.has(zone, "www.example.com A 1.1.1.1") || !h.has(zone, "www.example.com A 9.9.9.9") {
		t.Errorf("www not updated: %v", h.dns(zone))
	}
	if h.has(zone, "*.apps.example.com A 1.1.1.1") || !h.has(zone, "*.apps.example.com CNAME lb.example.net.") {
		t.Errorf("A to CNAME swap failed: %v", h.dns(zone))
	}

	// 3. Remove everything.
	h.converge(eps())
	for _, z := range []string{zone, "example.org"} {
		if left := h.dns(z); len(left) != 0 {
			t.Fatalf("records left behind in %s: %v", z, left)
		}
	}
}

// A fresh Hostinger zone has an apex A record and a www CNAME. external-dns
// does not own them, so it must never change them.
func TestFreshZoneDefaultsAreLeftAlone(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil)
	before := h.dns("fresh.net")
	if len(before) != 2 {
		t.Fatalf("fresh zone: %v", before)
	}
	h.converge(eps(endpoint.NewEndpoint("app.fresh.net", "A", "1.1.1.1")))
	if !h.has("fresh.net", "app.fresh.net A 1.1.1.1") {
		t.Fatalf("record not created: %v", h.dns("fresh.net"))
	}
	h.converge(eps())
	if got := h.dns("fresh.net"); strings.Join(got, "|") != strings.Join(before, "|") {
		t.Fatalf("default records changed:\n%v\nwant\n%v", got, before)
	}
}

// CAA is left out on purpose: the TXT registry of external-dns v0.24 cannot
// name ownership records for CAA (see registry/mapper), so external-dns
// would create a CAA record but never clean it up.
func TestOtherRecordTypes(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil, "A", "TXT", "MX", "SRV")
	h.converge(eps(
		endpoint.NewEndpoint("mail.example.com", "MX", "10 mx1.example.net", "20 mx2.example.net."),
		endpoint.NewEndpoint("_sip._tcp.example.com", "SRV", "10 5 5060 sip.example.net"),
		endpoint.NewEndpoint("spf.example.com", "TXT", "v=spf1 -all"),
	))
	for _, want := range []string{
		"mail.example.com MX 10 mx1.example.net.", "mail.example.com MX 20 mx2.example.net.",
		"_sip._tcp.example.com SRV 10 5 5060 sip.example.net.",
		`spf.example.com TXT "v=spf1 -all"`,
	} {
		if !h.has(zone, want) {
			t.Errorf("missing %q in %v", want, h.dns(zone))
		}
	}
	h.converge(eps())
	if left := h.dns(zone); len(left) != 0 {
		t.Fatalf("records left behind: %v", left)
	}
}

func TestForeignRecordsAreLeftAlone(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil)
	h.api.Seed(zone, hostingertest.RRset{Name: "manual", Type: "A", TTL: 300, Records: []hostingertest.Record{{Content: "4.4.4.4"}}})
	h.api.Seed(zone, hostingertest.RRset{Name: "@", Type: "MX", TTL: 300, Records: []hostingertest.Record{{Content: "10 mail.example.com."}}})
	h.api.Seed(zone, hostingertest.RRset{Name: "@", Type: "TXT", TTL: 300, Records: []hostingertest.Record{{Content: `"google-site-verification=abc"`}}})

	h.converge(eps(endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1"), endpoint.NewEndpoint("example.com", "A", "1.1.1.1")))
	h.converge(eps())
	for _, want := range []string{"manual.example.com A 4.4.4.4", "example.com MX 10 mail.example.com.", `example.com TXT "google-site-verification=abc"`} {
		if !h.has(zone, want) {
			t.Fatalf("records not owned by external-dns were changed, %q is missing: %v", want, h.dns(zone))
		}
	}
	if len(h.dns(zone)) != 3 {
		t.Fatalf("records left behind: %v", h.dns(zone))
	}
}

func TestUnusableTargetsDoNotLoop(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil)
	desired := eps(
		endpoint.NewEndpoint("mixed.example.com", "A", "1.1.1.1", "not-an-ip", "1.1.1.1"),
		endpoint.NewEndpoint("private.example.com", "A", "10.0.0.5"),
		endpoint.NewEndpointWithTTL("ttl.example.com", "A", 10, "1.1.1.1"),
		endpoint.NewEndpoint("bad name.example.com", "A", "1.1.1.1"),
	)
	h.converge(desired)
	for _, want := range []string{"mixed.example.com A 1.1.1.1", "ttl.example.com A 1.1.1.1", "private.example.com A 10.0.0.5"} {
		if !h.has(zone, want) {
			t.Fatalf("usable targets should be created, %q is missing: %v", want, h.dns(zone))
		}
	}
	if n := h.run(desired); n != 0 {
		t.Fatalf("third run planned %d changes", n)
	}
}

func TestDomainFilter(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, []string{"example.org"})
	h.converge(eps(
		endpoint.NewEndpoint("www.example.org", "A", "1.1.1.1"),
		endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1"),
	))
	if len(h.dns(zone)) != 0 {
		t.Fatalf("example.com is outside the filter but was written: %v", h.dns(zone))
	}
	if !h.has("example.org", "www.example.org A 1.1.1.1") {
		t.Fatal("example.org record missing")
	}
}

func TestUpsertOnlyNeverDeletes(t *testing.T) {
	h := newHarness(t, &plan.UpsertOnlyPolicy{}, nil)
	h.converge(eps(endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1")))
	if n := h.run(eps()); n != 0 {
		t.Fatalf("upsert-only planned %d changes for a removal", n)
	}
	if !h.has(zone, "www.example.com A 1.1.1.1") {
		t.Fatal("upsert-only deleted a record")
	}
}

func TestTransientErrorsRecover(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil)
	desired := eps(endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1"))
	// Every write fails with a 503 for a while. The webhook returns 500 and
	// external-dns tries again on the next loop.
	h.api.Inject(hostingertest.Fault{Method: "PUT", Status: 503, Count: 3})
	h.run(desired)
	if h.errs == 0 {
		t.Fatal("expected the first loop to fail")
	}
	h.errs = 0
	for range 3 {
		if h.run(desired) == 0 {
			break
		}
	}
	if !h.has(zone, "www.example.com A 1.1.1.1") {
		t.Fatalf("record not created after the outage: %v", h.dns(zone))
	}
	if n := h.run(desired); n != 0 {
		t.Fatalf("not stable after recovery: %d changes", n)
	}
}

func TestRateLimitIsRespected(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil)
	// Two 429s in a row. Retry-After is 0 so the test does not wait a
	// minute; the client must retry and the loop must still converge.
	h.api.Inject(hostingertest.Fault{Status: 429, Count: 2, Header: map[string][]string{"Retry-After": {"0"}}})
	h.converge(eps(endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1")))
}

// An outage of the DELETE call: the record must keep its ownership record,
// so that external-dns still owns it and deletes it once the API recovers.
func TestDeleteOutageLeavesNothingBehind(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil)
	h.converge(eps(
		endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1"),
		endpoint.NewEndpoint("*.apps.example.com", "A", "1.1.1.1"),
	))
	// Make every delete of the wildcard record fail.
	h.api.Inject(hostingertest.Fault{Method: "DELETE", BodyContains: `"name":"*.apps"`, Status: 500, Count: 1000})
	h.run(eps())
	if h.errs == 0 {
		t.Fatal("expected the delete to fail")
	}
	if !h.has(zone, "*.apps.example.com A 1.1.1.1") {
		t.Fatal("the wildcard record should still be there during the outage")
	}
	owned := false
	for _, l := range h.dns(zone) {
		if strings.HasPrefix(l, "extdns-a.wildcard.apps.example.com TXT") {
			owned = true
		}
	}
	if !owned {
		t.Fatalf("the ownership record of the wildcard was deleted, so it is orphaned: %v", h.dns(zone))
	}

	h.api.ClearFaults()
	h.errs = 0
	h.run(eps())
	if left := h.dns(zone); len(left) != 0 {
		t.Fatalf("records left after the outage: %v", left)
	}
	if n := h.run(eps()); n != 0 || h.errs != 0 {
		t.Fatalf("not stable after recovery: %d changes, %d errors", n, h.errs)
	}
}
