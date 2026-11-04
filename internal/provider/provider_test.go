package provider

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/hayone1/external-dns-hostinger-webhook/internal/hostinger"
	"github.com/hayone1/external-dns-hostinger-webhook/internal/hostinger/hostingertest"
)

const zone = "example.com"

func setup(t *testing.T, o Options) (*hostingertest.Server, *Provider) {
	t.Helper()
	api := hostingertest.New()
	t.Cleanup(func() {
		if n := api.ForbiddenCalls(); n != 0 {
			t.Errorf("the webhook called zone reset or snapshot restore %d times", n)
		}
		api.Close()
	})
	api.AddBareZone(zone)
	c, err := hostinger.New(hostinger.Options{APIURL: api.URL, Token: hostingertest.Token, RateLimit: 1000, Burst: 100, MaxRetries: 2, BaseBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if o.DefaultTTL == 0 {
		o.DefaultTTL = 300
	}
	return api, New(c, o)
}

func values(api *hostingertest.Server, z, name, typ string) []string {
	for _, s := range api.RRsets(z) {
		if s.Name == name && s.Type == typ {
			v := s.Values()
			slices.Sort(v)
			return v
		}
	}
	return nil
}

func ttlOf(api *hostingertest.Server, z, name, typ string) int64 {
	for _, s := range api.RRsets(z) {
		if s.Name == name && s.Type == typ {
			return s.TTL
		}
	}
	return -1
}

func rrset(name, typ string, ttl int64, vals ...string) hostingertest.RRset {
	s := hostingertest.RRset{Name: name, Type: typ, TTL: ttl}
	for _, v := range vals {
		s.Records = append(s.Records, hostingertest.Record{Content: v})
	}
	return s
}

func owner(name, typ string) *endpoint.Endpoint {
	return endpoint.NewEndpoint(name, endpoint.RecordTypeTXT, `"heritage=external-dns,external-dns/owner=test,external-dns/resource=service/default/`+typ+`"`)
}

func TestRecords(t *testing.T) {
	api, p := setup(t, Options{})
	api.AddBareZone("dev.example.com")
	api.Seed(zone, rrset("@", "A", 50, "2.57.91.91"))
	api.Seed(zone, rrset("www", "A", 300, "8.8.8.8", "1.1.1.1"))
	api.Seed(zone, rrset("www", "TXT", 300, `"hello"`))
	api.Seed(zone, rrset("@", "MX", 3600, "10 mail.example.com."))
	api.Seed(zone, rrset("alias", "CNAME", 300, "Target.Example.NET."))
	api.Seed(zone, rrset("@", "ALIAS", 300, "lb.example.net."))
	api.Seed(zone, rrset("sub", "NS", 300, "ns1.example.net."))
	api.Seed(zone, rrset("x.dev", "A", 300, "9.9.9.9")) // belongs to the child zone, never served
	api.Seed("dev.example.com", rrset("api", "A", 300, "4.4.4.4"))
	off := rrset("half", "A", 300, "1.1.1.1", "2.2.2.2")
	off.Records[1].IsDisabled = true
	api.Seed(zone, off)
	allOff := rrset("off", "A", 300, "3.3.3.3")
	allOff.Records[0].IsDisabled = true
	api.Seed(zone, allOff)

	eps, err := p.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ep := range eps {
		got = append(got, ep.DNSName+" "+ep.RecordType+" "+strings.Join(ep.Targets, ",")+" "+strconv.FormatInt(int64(ep.RecordTTL), 10))
	}
	want := []string{
		"alias.example.com CNAME target.example.net 300",
		"api.dev.example.com A 4.4.4.4 300",
		"example.com A 2.57.91.91 50",
		"example.com MX 10 mail.example.com 3600",
		"half.example.com A 1.1.1.1 300",
		"www.example.com A 1.1.1.1,8.8.8.8 300",
		`www.example.com TXT "hello" 300`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDomainFilterAndZoneCache(t *testing.T) {
	api, p := setup(t, Options{DomainFilter: endpoint.NewDomainFilter([]string{"app.example.com"})})
	api.AddBareZone("example.org")
	api.Seed(zone, rrset("www", "A", 300, "1.1.1.1"))
	api.Seed(zone, rrset("x.app", "A", 300, "1.1.1.1"))
	api.Seed("example.org", rrset("www", "A", 300, "1.1.1.1"))
	ctx := context.Background()
	for range 3 {
		eps, err := p.Records(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(eps) != 1 || eps[0].DNSName != "x.app.example.com" {
			t.Fatalf("filter not applied: %v", eps)
		}
	}
	if n := api.Calls("listDomains"); n != 1 {
		t.Errorf("the domain list must be cached, it was read %d times", n)
	}
	if api.Calls("getZone") != 3 {
		t.Errorf("only the zone that matches the filter is read: %d calls", api.Calls("getZone"))
	}
}

func TestExtraZones(t *testing.T) {
	api, p := setup(t, Options{ExtraZones: []string{"elsewhere.net", "app.example.com", "missing.org"}})
	api.AddForeignZone("elsewhere.net")
	api.Seed("elsewhere.net", rrset("www", "A", 300, "1.1.1.1"))
	_, all, err := p.Zones(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(all, ",") != "elsewhere.net,example.com,missing.org" {
		t.Fatalf("zones = %v (app.example.com is covered by example.com and must not be a zone)", all)
	}
	eps, err := p.Records(context.Background())
	if err != nil {
		t.Fatalf("a zone the account does not own must be skipped, not fail the read: %v", err)
	}
	if len(eps) != 1 || eps[0].DNSName != "www.elsewhere.net" {
		t.Fatalf("records = %v", eps)
	}
}

func TestInactiveDomainsAreSkipped(t *testing.T) {
	api, p := setup(t, Options{})
	api.AddBareZone("expired.org")
	api.SetDomainStatus("expired.org", "expired")
	_, all, err := p.Zones(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(all, ",") != "example.com" {
		t.Fatalf("an expired domain must not be read: %v", all)
	}

	// Naming it in DOMAIN_FILTER still manages it.
	api2, p3 := setup(t, Options{ExtraZones: []string{"expired.org"}})
	api2.AddBareZone("expired.org")
	api2.SetDomainStatus("expired.org", "expired")
	_, all, err = p3.Zones(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(all, ",") != "example.com,expired.org" {
		t.Fatalf("a domain named in DOMAIN_FILTER must be kept: %v", all)
	}
}

func TestReadErrorsAreNotHidden(t *testing.T) {
	api, p := setup(t, Options{})
	api.Inject(hostingertest.Fault{Method: "GET", Path: "/zones/", Status: 500, Count: 10})
	if _, err := p.Records(context.Background()); err == nil {
		t.Fatal("a failed zone read must fail Records, or external-dns would think the zone is empty")
	}
}

func TestAdjustEndpoints(t *testing.T) {
	_, p := setup(t, Options{DefaultTTL: 600})
	in := []*endpoint.Endpoint{
		endpoint.NewEndpoint("WWW.Example.com.", "A", "8.8.8.8", "1.1.1.1", "::ffff:1.1.1.1", "bad"),
		endpoint.NewEndpointWithTTL("short.example.com", "A", 5, "10.0.0.1"),
		endpoint.NewEndpointWithTTL("long.example.com", "A", 604800, "1.1.1.1"),
		endpoint.NewEndpoint("c.example.com", "CNAME", "B.example.net.", "a.example.net"),
		endpoint.NewEndpoint("t.example.com", "TXT", "v=spf1 -all"),
		endpoint.NewEndpoint("ns.example.com", "NS", "ns1.example.net"),
		endpoint.NewEndpoint("ptr.example.com", "PTR", "x.example.net"),
		endpoint.NewEndpoint("extdns-a.*.wild.example.com", "TXT", "x"),
		endpoint.NewEndpoint("empty.example.com", "A", "nope"),
		endpoint.NewEndpoint("set.example.com", "A", "1.1.1.1").WithSetIdentifier("eu"),
		endpoint.NewEndpoint("*.apps.example.com", "A", "1.1.1.1"),
	}
	in[0].ProviderSpecific = endpoint.ProviderSpecific{{Name: "x", Value: "y"}}
	out, err := p.AdjustEndpoints(in)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ep := range out {
		got = append(got, ep.DNSName+" "+ep.RecordType+" "+strings.Join(ep.Targets, ",")+" "+strconv.FormatInt(int64(ep.RecordTTL), 10))
		if ep.ProviderSpecific != nil {
			t.Errorf("provider specific must be cleared on %s", ep.DNSName)
		}
	}
	want := []string{
		"www.example.com A 1.1.1.1,8.8.8.8 600",
		"short.example.com A 10.0.0.1 60",
		"long.example.com A 1.1.1.1 86400",
		"c.example.com CNAME a.example.net 600",
		`t.example.com TXT "v=spf1 -all" 600`,
		"*.apps.example.com A 1.1.1.1 600",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("adjusted:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func apply(t *testing.T, p *Provider, c *plan.Changes) error {
	t.Helper()
	return p.ApplyChanges(context.Background(), c)
}

func TestCreateKeepsExistingValues(t *testing.T) {
	api, p := setup(t, Options{})
	api.Seed(zone, rrset("www", "A", 300, "1.1.1.1"))
	err := apply(t, p, &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.com", "A", "8.8.8.8", "1.1.1.1"),
		endpoint.NewEndpointWithTTL("new.example.com", "A", 120, "9.9.9.9"),
		endpoint.NewEndpoint("example.com", "TXT", "apex text"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if v := values(api, zone, "www", "A"); strings.Join(v, ",") != "1.1.1.1,8.8.8.8" {
		t.Errorf("create must add to the RRset, not replace it: %v", v)
	}
	if v := values(api, zone, "new", "A"); strings.Join(v, ",") != "9.9.9.9" || ttlOf(api, zone, "new", "A") != 120 {
		t.Errorf("new RRset: %v ttl %d", v, ttlOf(api, zone, "new", "A"))
	}
	if v := values(api, zone, "@", "TXT"); strings.Join(v, ",") != `"apex text"` {
		t.Errorf("apex TXT: %v", v)
	}
	if n := api.Calls("updateZone"); n != 1 {
		t.Errorf("all creates of one phase go in one PUT, got %d", n)
	}

	// The same create again changes nothing and sends nothing.
	before := api.MutatingCalls()
	if err := apply(t, p, &plan.Changes{Create: []*endpoint.Endpoint{endpoint.NewEndpoint("new.example.com", "A", "9.9.9.9")}}); err != nil {
		t.Fatal(err)
	}
	if api.MutatingCalls() != before {
		t.Error("a create of an existing value must be a no-op")
	}
}

func TestCreateNeverOverwritesAForeignCNAME(t *testing.T) {
	api, p := setup(t, Options{})
	api.Seed(zone, rrset("c", "CNAME", 300, "theirs.example.net."))
	err := apply(t, p, &plan.Changes{Create: []*endpoint.Endpoint{endpoint.NewEndpoint("c.example.com", "CNAME", "ours.example.net")}})
	if err == nil || !strings.Contains(err.Error(), "does not own") {
		t.Fatalf("expected a clear error, got %v", err)
	}
	if v := values(api, zone, "c", "CNAME"); strings.Join(v, ",") != "theirs.example.net." || api.MutatingCalls() != 0 {
		t.Fatalf("the foreign CNAME was changed: %v", v)
	}
}

func TestUpdateKeepsValuesAddedByHand(t *testing.T) {
	api, p := setup(t, Options{})
	api.Seed(zone, rrset("www", "A", 300, "1.1.1.1", "2.2.2.2", "3.3.3.3"))
	api.Seed(zone, rrset("c", "CNAME", 300, "old.example.net."))
	err := apply(t, p, &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{
			endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1", "2.2.2.2"),
			endpoint.NewEndpoint("c.example.com", "CNAME", "old.example.net"),
		},
		UpdateNew: []*endpoint.Endpoint{
			endpoint.NewEndpointWithTTL("www.example.com", "A", 600, "2.2.2.2", "4.4.4.4"),
			endpoint.NewEndpoint("c.example.com", "CNAME", "new.example.net"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v := values(api, zone, "www", "A"); strings.Join(v, ",") != "2.2.2.2,3.3.3.3,4.4.4.4" || ttlOf(api, zone, "www", "A") != 600 {
		t.Errorf("update: %v ttl %d (3.3.3.3 was added by hand and must stay)", v, ttlOf(api, zone, "www", "A"))
	}
	if v := values(api, zone, "c", "CNAME"); strings.Join(v, ",") != "new.example.net." {
		t.Errorf("CNAME update: %v", v)
	}
	if n := api.Calls("updateZone"); n != 1 {
		t.Errorf("updates go in one PUT, got %d", n)
	}
}

func TestDeleteRemovesOnlyKnownValues(t *testing.T) {
	api, p := setup(t, Options{})
	api.Seed(zone, rrset("www", "A", 300, "1.1.1.1", "2.2.2.2"))
	api.Seed(zone, rrset("@", "A", 50, "2.57.91.91", "1.1.1.1"))
	api.Seed(zone, rrset("gone", "A", 300, "1.1.1.1"))
	api.Seed(zone, rrset("*.apps", "A", 300, "1.1.1.1"))
	err := apply(t, p, &plan.Changes{Delete: []*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1"),
		endpoint.NewEndpoint("example.com", "A", "1.1.1.1"),
		endpoint.NewEndpoint("gone.example.com", "A", "1.1.1.1"),
		endpoint.NewEndpoint("*.apps.example.com", "A", "1.1.1.1"),
		endpoint.NewEndpoint("never.example.com", "A", "1.1.1.1"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if v := values(api, zone, "www", "A"); strings.Join(v, ",") != "2.2.2.2" {
		t.Errorf("partial delete: %v", v)
	}
	// The apex keeps its parking value; its TTL of 50 is below what the API
	// accepts on writes, so it is raised to 60.
	if v := values(api, zone, "@", "A"); strings.Join(v, ",") != "2.57.91.91" || ttlOf(api, zone, "@", "A") != 60 {
		t.Errorf("apex partial delete: %v ttl %d", v, ttlOf(api, zone, "@", "A"))
	}
	if values(api, zone, "gone", "A") != nil || values(api, zone, "*.apps", "A") != nil {
		t.Errorf("full deletes: %v", api.RRsets(zone))
	}
	if api.Calls("deleteRRsets") != 1 || api.Calls("updateZone") != 1 {
		t.Errorf("one DELETE and one PUT expected, got %d and %d", api.Calls("deleteRRsets"), api.Calls("updateZone"))
	}
}

func TestOneBadRRsetDoesNotBlockTheRest(t *testing.T) {
	api, p := setup(t, Options{})
	api.Seed(zone, rrset("www", "CNAME", 300, "example.com."))
	var creates []*endpoint.Endpoint
	for _, n := range []string{"a", "b", "c", "www", "d", "e", "f"} {
		creates = append(creates, endpoint.NewEndpoint(n+".example.com", "A", "1.1.1.1"))
	}
	err := apply(t, p, &plan.Changes{Create: creates})
	if err == nil || !strings.Contains(err.Error(), "www.example.com A") || !strings.Contains(err.Error(), "must not be used with any other type") {
		t.Fatalf("expected the www error, got %v", err)
	}
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		if values(api, zone, n, "A") == nil {
			t.Errorf("%s was not written", n)
		}
	}
	// 7 sets, one bad: the split needs far fewer calls than one per set.
	if n := api.Calls("updateZone"); n > 7 {
		t.Errorf("%d PUT calls; splitting should find the bad set faster", n)
	}
}

func TestTemporaryErrorsAreNotSplit(t *testing.T) {
	api, p := setup(t, Options{})
	api.Inject(hostingertest.Fault{Method: "PUT", Status: 503, Count: 100})
	err := apply(t, p, &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpoint("a.example.com", "A", "1.1.1.1"),
		endpoint.NewEndpoint("b.example.com", "A", "1.1.1.1"),
	}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := api.Calls("updateZone"); n != 3 {
		t.Errorf("one batch with 2 retries expected, got %d PUTs", n)
	}
}

func TestOwnershipIsKeptWhenTheRecordDeleteFails(t *testing.T) {
	api, p := setup(t, Options{})
	api.Seed(zone, rrset("www", "A", 300, "1.1.1.1"))
	api.Seed(zone, rrset("extdns-a.www", "TXT", 300, `"heritage=external-dns,external-dns/owner=test,external-dns/resource=service/default/web"`))
	api.Inject(hostingertest.Fault{Method: "DELETE", BodyContains: `"name":"www"`, Status: 500, Count: 100})
	rec := endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1")
	rec.Labels[endpoint.ResourceLabelKey] = "service/default/web"
	err := apply(t, p, &plan.Changes{Delete: []*endpoint.Endpoint{rec, owner("extdns-a.www.example.com", "web")}})
	if err == nil {
		t.Fatal("expected the delete to fail")
	}
	if values(api, zone, "extdns-a.www", "TXT") == nil {
		t.Fatal("the ownership record was deleted although its record is still there")
	}
}

func TestRecordIsNotCreatedWithoutItsOwner(t *testing.T) {
	api, p := setup(t, Options{})
	api.Inject(hostingertest.Fault{Method: "PUT", BodyContains: "heritage=external-dns", Status: 500, Count: 100})
	rec := endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1")
	rec.Labels[endpoint.ResourceLabelKey] = "service/default/web"
	err := apply(t, p, &plan.Changes{Create: []*endpoint.Endpoint{owner("extdns-a.www.example.com", "web"), rec}})
	if err == nil || !strings.Contains(err.Error(), "its ownership record") {
		t.Fatalf("expected an ownership error, got %v", err)
	}
	if values(api, zone, "www", "A") != nil {
		t.Fatal("a record without its ownership record would be orphaned")
	}
}

func TestBadNamesAreRefusedLocally(t *testing.T) {
	api, p := setup(t, Options{})
	err := apply(t, p, &plan.Changes{Create: []*endpoint.Endpoint{endpoint.NewEndpoint("extdns-a.*.wild.example.com", "TXT", `"heritage=external-dns"`)}})
	if err == nil || !strings.Contains(err.Error(), "--txt-wildcard-replacement") {
		t.Fatalf("expected a hint, got %v", err)
	}
	if api.MutatingCalls() != 0 {
		t.Fatal("a name Hostinger refuses must not be sent")
	}
}

func TestDisabledValuesBlockEdits(t *testing.T) {
	api, p := setup(t, Options{})
	s := rrset("www", "A", 300, "1.1.1.1", "2.2.2.2")
	s.Records[1].IsDisabled = true
	api.Seed(zone, s)
	err := apply(t, p, &plan.Changes{Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1")}})
	if err == nil || !strings.Contains(err.Error(), "disabled in hPanel") {
		t.Fatalf("expected a disabled error, got %v", err)
	}
	if api.MutatingCalls() != 0 {
		t.Fatal("nothing may be written")
	}
}

func TestNoZone(t *testing.T) {
	api, p := setup(t, Options{})
	err := apply(t, p, &plan.Changes{Create: []*endpoint.Endpoint{endpoint.NewEndpoint("www.other.org", "A", "1.1.1.1")}})
	if err != nil || api.MutatingCalls() != 0 {
		t.Fatalf("a name outside every zone is skipped: %v", err)
	}
}

func TestDryRun(t *testing.T) {
	api, p := setup(t, Options{DryRun: true})
	api.Seed(zone, rrset("old", "A", 300, "1.1.1.1"))
	err := apply(t, p, &plan.Changes{
		Create: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1")},
		Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("old.example.com", "A", "1.1.1.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if api.MutatingCalls() != 0 {
		t.Fatal("dry run must not write")
	}
	if api.Calls("validateZone") != 1 {
		t.Errorf("dry run validates the PUT: %d calls", api.Calls("validateZone"))
	}
}

func TestSameKeyTwiceInOnePhase(t *testing.T) {
	api, p := setup(t, Options{})
	err := apply(t, p, &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1"),
		endpoint.NewEndpoint("WWW.example.com.", "A", "8.8.8.8"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if v := values(api, zone, "www", "A"); strings.Join(v, ",") != "1.1.1.1,8.8.8.8" {
		t.Fatalf("two creates of one RRset must merge: %v", v)
	}
}
