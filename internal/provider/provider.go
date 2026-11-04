// Package provider implements the external-dns provider.Provider interface on
// top of the Hostinger DNS API.
package provider

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/provider"

	"github.com/hayone1/external-dns-hostinger-webhook/internal/hostinger"
	"github.com/hayone1/external-dns-hostinger-webhook/internal/metrics"
)

// zoneReadConcurrency limits how many zones are read at the same time. The
// client rate limiter still applies on top of this.
const zoneReadConcurrency = 2

// DefaultZoneCacheTTL is how long the list of domains is kept before it is
// read again. The domain list changes rarely, and every call counts against
// Hostinger's rate limit of 90 requests per minute.
const DefaultZoneCacheTTL = 5 * time.Minute

// API is the part of the Hostinger client the provider uses. Tests replace
// it with an in-memory fake.
type API interface {
	ListDomains(ctx context.Context) ([]hostinger.Domain, error)
	GetZone(ctx context.Context, domain string) ([]hostinger.RRset, error)
	UpdateZone(ctx context.Context, domain string, sets []hostinger.RRset) error
	ValidateZone(ctx context.Context, domain string, sets []hostinger.RRset) error
	DeleteRRsets(ctx context.Context, domain string, filters []hostinger.Filter) error
}

// Options configures a Provider.
type Options struct {
	DomainFilter *endpoint.DomainFilter
	// ExtraZones are zone names to manage even if they are not in the
	// domain portfolio, for example a domain registered elsewhere whose
	// DNS is hosted at Hostinger. The webhook passes the plain
	// DOMAIN_FILTER entries here.
	ExtraZones   []string
	DefaultTTL   int64
	DryRun       bool
	ZoneCacheTTL time.Duration
	Logger       *slog.Logger
	Metrics      *metrics.Metrics
}

// Provider is the Hostinger implementation of provider.Provider.
type Provider struct {
	provider.BaseProvider

	api          API
	domainFilter *endpoint.DomainFilter
	extraZones   []string
	defaultTTL   int64
	dryRun       bool
	log          *slog.Logger
	metrics      *metrics.Metrics

	// applyMu makes sure two ApplyChanges calls never interleave. external-dns
	// does not send them in parallel, but a retry after a timeout could.
	// Hostinger has no ETag or version on zones, so reading and writing a
	// zone close together under this lock is the best protection there is.
	applyMu sync.Mutex

	cacheTTL  time.Duration
	cacheMu   sync.Mutex
	cached    []string
	cachedAt  time.Time
	now       func() time.Time
	warnedMu  sync.Mutex
	warnedFor map[string]bool
}

var _ provider.Provider = (*Provider)(nil)

// New creates a Provider.
func New(api API, o Options) *Provider {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.DomainFilter == nil {
		o.DomainFilter = endpoint.NewDomainFilter(nil)
	}
	if o.DefaultTTL <= 0 {
		o.DefaultTTL = 3600
	}
	o.DefaultTTL = clampTTL(o.DefaultTTL)
	if o.ZoneCacheTTL < 0 {
		o.ZoneCacheTTL = 0
	} else if o.ZoneCacheTTL == 0 {
		o.ZoneCacheTTL = DefaultZoneCacheTTL
	}
	var extra []string
	for _, z := range o.ExtraZones {
		if z = normalizeName(z); z != "" && !slices.Contains(extra, z) {
			extra = append(extra, z)
		}
	}
	return &Provider{
		api:          api,
		domainFilter: o.DomainFilter,
		extraZones:   extra,
		defaultTTL:   o.DefaultTTL,
		dryRun:       o.DryRun,
		log:          o.Logger,
		metrics:      o.Metrics,
		cacheTTL:     o.ZoneCacheTTL,
		now:          time.Now,
		warnedFor:    map[string]bool{},
	}
}

// GetDomainFilter returns the filter external-dns should apply.
func (p *Provider) GetDomainFilter() endpoint.DomainFilterInterface {
	return p.domainFilter
}

// zoneState is one zone with its current record sets.
type zoneState struct {
	name string
	sets []hostinger.RRset
}

// zones returns every zone the account can hold: the domain portfolio plus
// the extra zones. The list is cached for a few minutes.
func (p *Provider) zones(ctx context.Context) ([]string, error) {
	p.cacheMu.Lock()
	if p.cached != nil && p.now().Sub(p.cachedAt) < p.cacheTTL {
		out := slices.Clone(p.cached)
		p.cacheMu.Unlock()
		return out, nil
	}
	p.cacheMu.Unlock()

	domains, err := p.api.ListDomains(ctx)
	if err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}
	var all []string
	for _, d := range domains {
		if d.Domain == nil {
			continue // an unclaimed free domain has no name yet
		}
		if name := normalizeName(*d.Domain); name != "" && !slices.Contains(all, name) {
			all = append(all, name)
		}
	}
	// An extra zone is only needed when no portfolio domain already covers
	// it. DOMAIN_FILTER=app.example.com must not turn into a zone of its
	// own when example.com is in the portfolio.
	for _, z := range p.extraZones {
		if _, covered := zoneFor(z, all); !covered {
			all = append(all, z)
		}
	}
	sort.Strings(all)

	p.cacheMu.Lock()
	p.cached, p.cachedAt = slices.Clone(all), p.now()
	p.cacheMu.Unlock()
	return all, nil
}

// managedZones lists the zones whose names or sub-names match the domain
// filter. It returns the normalized names of all zones too, so that records
// can be matched to the most specific zone.
func (p *Provider) managedZones(ctx context.Context) (managed, all []string, err error) {
	all, err = p.zones(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range all {
		// A zone is managed if the filter matches it, or if the filter
		// names a sub-domain of it (DOMAIN_FILTER=app.example.com still
		// needs the example.com zone).
		if p.domainFilter.Match(name) || p.domainFilter.MatchParent(name) {
			managed = append(managed, name)
		}
	}
	return managed, all, nil
}

// Zones returns the zones the webhook manages and all zones it can see.
// The check command uses it.
func (p *Provider) Zones(ctx context.Context) (managed, all []string, err error) {
	return p.managedZones(ctx)
}

// readZones fetches the record sets of several zones at once. A zone that
// is not in the account (HTTP 403) is skipped with a warning; any other
// error fails the whole read, because a missing zone would look like a
// zone without records.
func (p *Provider) readZones(ctx context.Context, names []string) ([]zoneState, error) {
	out := make([]*zoneState, len(names))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(zoneReadConcurrency)
	for i, name := range names {
		g.Go(func() error {
			sets, err := p.api.GetZone(gctx, name)
			if hostinger.IsNotOwned(err) {
				p.warnOnce("not-owned/"+name, "skipping zone: Hostinger says this account does not own it",
					"zone", name, "error", err)
				return nil
			}
			if err != nil {
				return fmt.Errorf("read zone %s: %w", name, err)
			}
			out[i] = &zoneState{name: name, sets: sets}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	states := make([]zoneState, 0, len(out))
	for _, s := range out {
		if s != nil {
			states = append(states, *s)
		}
	}
	return states, nil
}

func (p *Provider) warnOnce(key, msg string, args ...any) {
	p.warnedMu.Lock()
	seen := p.warnedFor[key]
	p.warnedFor[key] = true
	p.warnedMu.Unlock()
	if seen {
		p.log.Debug(msg, args...)
		return
	}
	p.log.Warn(msg, args...)
}

// Records returns every record set in the managed zones.
func (p *Provider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	managed, all, err := p.managedZones(ctx)
	if err != nil {
		return nil, err
	}
	states, err := p.readZones(ctx, managed)
	if err != nil {
		return nil, err
	}

	var endpoints []*endpoint.Endpoint
	for _, zs := range states {
		for _, set := range zs.sets {
			if !supportedType(set.Type) {
				continue // NS, SOA, ALIAS: managed by Hostinger or by hand
			}
			fqdn := recordFQDN(set.Name, zs.name)
			// Skip records that sit in a parent zone but belong to a more
			// specific child zone; DNS never serves them.
			if owner, _ := zoneFor(fqdn, all); owner != zs.name {
				continue
			}
			if !p.domainFilter.Match(fqdn) {
				continue
			}
			var targets []string
			for _, r := range set.Records {
				if r.IsDisabled {
					continue // stored but not served, so it does not exist for external-dns
				}
				t, ok := recordTarget(set.Type, r.Content)
				if ok && !slices.Contains(targets, t) {
					targets = append(targets, t)
				}
			}
			if len(targets) == 0 {
				continue
			}
			sort.Strings(targets)
			ep := endpoint.NewEndpointWithTTL(fqdn, set.Type, endpoint.TTL(set.TTL), targets...)
			if ep == nil {
				p.log.Warn("skipping record with a label longer than 63 characters", "name", fqdn, "type", set.Type)
				continue
			}
			endpoints = append(endpoints, ep)
		}
	}
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].DNSName != endpoints[j].DNSName {
			return endpoints[i].DNSName < endpoints[j].DNSName
		}
		return endpoints[i].RecordType < endpoints[j].RecordType
	})

	p.metrics.SetInventory(len(states), len(endpoints))
	p.metrics.MarkSuccess()
	p.log.Debug("read records from Hostinger", "zones", len(states), "endpoints", len(endpoints))
	return endpoints, nil
}

// AdjustEndpoints rewrites the desired endpoints so they look exactly like
// what Records would return after they are created. That stops external-dns
// from planning the same change on every loop. Endpoints Hostinger cannot
// store are dropped with a warning instead of failing the whole batch.
func (p *Provider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	out := make([]*endpoint.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if ep == nil {
			continue
		}
		ep.DNSName = normalizeName(ep.DNSName)
		log := p.log.With("name", ep.DNSName, "type", ep.RecordType)

		if !supportedType(ep.RecordType) {
			log.Warn("dropping endpoint: this webhook cannot manage this record type")
			p.metrics.ObserveSkipped("unsupported_type")
			continue
		}
		if ep.SetIdentifier != "" {
			log.Warn("dropping endpoint: Hostinger has no routing policies, so set-identifier is not supported", "setIdentifier", ep.SetIdentifier)
			p.metrics.ObserveSkipped("set_identifier")
			continue
		}
		if err := checkName(ep.DNSName, ""); err != nil {
			log.Warn("dropping endpoint: Hostinger would refuse the name", "error", err)
			p.metrics.ObserveSkipped("invalid_name")
			continue
		}

		targets := make([]string, 0, len(ep.Targets))
		for _, t := range ep.Targets {
			content, err := contentFor(ep.DNSName, ep.RecordType, t)
			if err != nil {
				log.Warn("dropping target", "target", t, "error", err)
				p.metrics.ObserveSkipped("invalid_target")
				continue
			}
			canon, _ := recordTarget(ep.RecordType, content)
			// Hostinger refuses an RRset with the same value twice.
			if !slices.Contains(targets, canon) {
				targets = append(targets, canon)
			}
		}
		if len(targets) == 0 {
			log.Warn("dropping endpoint: no usable targets left")
			continue
		}
		sort.Strings(targets)
		if ep.RecordType == hostinger.TypeCNAME && len(targets) > 1 {
			log.Warn("a CNAME can only have one target, keeping the first", "targets", targets)
			p.metrics.ObserveSkipped("extra_cname_target")
			targets = targets[:1]
		}
		ep.Targets = targets

		ttl := p.defaultTTL
		if ep.RecordTTL.IsConfigured() && ep.RecordTTL > 0 {
			ttl = int64(ep.RecordTTL)
		}
		if c := clampTTL(ttl); c != ttl {
			log.Warn("TTL is outside what Hostinger accepts, using the nearest allowed value",
				"ttl", ttl, "used", c, "min", hostinger.MinTTL, "max", hostinger.MaxTTL)
			p.metrics.ObserveSkipped("ttl_clamped")
			ttl = c
		}
		ep.RecordTTL = endpoint.TTL(ttl)
		// Hostinger has no provider specific settings. Clearing them keeps
		// desired and current endpoints equal.
		ep.ProviderSpecific = nil
		out = append(out, ep)
	}
	return out, nil
}

// ttlOf returns the TTL to send for an endpoint.
func (p *Provider) ttlOf(ep *endpoint.Endpoint) int64 {
	if ep.RecordTTL.IsConfigured() && ep.RecordTTL > 0 {
		return clampTTL(int64(ep.RecordTTL))
	}
	return p.defaultTTL
}

// describe is used in log lines.
func describe(ep *endpoint.Endpoint) string {
	return fmt.Sprintf("%s %s [%s]", ep.DNSName, ep.RecordType, strings.Join(ep.Targets, ", "))
}
