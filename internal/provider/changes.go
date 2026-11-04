package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/hayone1/external-dns-hostinger-webhook/internal/hostinger"
)

// zoneChanges is the part of a plan.Changes that belongs to one zone.
type zoneChanges struct {
	create []*endpoint.Endpoint
	update []updatePair
	delete []*endpoint.Endpoint
}

type updatePair struct {
	old, new *endpoint.Endpoint
}

// zoneWork is one zone with the changes for it and its current record sets,
// kept up to date as the phases run.
type zoneWork struct {
	zone string
	zc   *zoneChanges
	sets map[endpointKey]*hostinger.RRset
}

// change is one endpoint change to plan in a phase.
type change struct {
	action string // create, update or delete
	ep     *endpoint.Endpoint
	old    *endpoint.Endpoint // update only
}

// op is one write to Hostinger: an RRset to put, or one to delete.
type op struct {
	key    endpointKey
	set    *hostinger.RRset // nil deletes the whole RRset
	action string
	eps    []*endpoint.Endpoint
}

func (o *op) describe(zone string) string {
	if o.set == nil {
		return fmt.Sprintf("%s %s", o.key.name, o.key.recordType)
	}
	vals := make([]string, 0, len(o.set.Records))
	for _, r := range o.set.Records {
		vals = append(vals, r.Content)
	}
	return fmt.Sprintf("%s %s ttl=%d [%s] in zone %s", o.key.name, o.key.recordType, o.set.TTL, strings.Join(vals, ", "), zone)
}

// ApplyChanges creates, updates and deletes records so that Hostinger
// matches the plan.
//
// It re-reads every affected zone first, so it always works on the real
// record sets, even if records were changed by hand since the last Records
// call. Hostinger stores whole record sets, so every change is turned into
// "this name and type now has exactly these values" (a PUT) or "this name
// and type is gone" (a DELETE). Values that external-dns did not know about
// are kept.
//
// The order of the work matters:
//
//  1. delete records
//  2. delete ownership TXT records, but keep the ones whose record could not
//     be deleted in step 1
//  3. update records
//  4. create ownership TXT records
//  5. create records, but skip the ones whose ownership record could not be
//     created in step 4
//
// Deleting first frees a name before a record of another type (for example
// a CNAME) takes it over. Steps 2 and 5 make sure a record never exists
// without the TXT record that tells external-dns it owns it. Without that, a
// failed delete would leave a record behind that external-dns could never
// clean up.
//
// Each step sends at most one DELETE and one PUT per zone. If Hostinger
// refuses a PUT (HTTP 422), the whole PUT is refused, so the webhook splits
// it in halves until it finds the bad record sets and writes the rest.
// One failing record does not stop the others. All errors are returned
// together at the end, so external-dns retries only what is still missing.
func (p *Provider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	if changes == nil || !changes.HasChanges() {
		return nil
	}
	p.applyMu.Lock()
	defer p.applyMu.Unlock()

	_, all, err := p.managedZones(ctx)
	if err != nil {
		return err
	}

	var errs []error
	byZone := map[string]*zoneChanges{}
	get := func(ep *endpoint.Endpoint) *zoneChanges {
		zone, ok := zoneFor(normalizeName(ep.DNSName), all)
		if !ok {
			args := []any{"name", ep.DNSName, "type", ep.RecordType}
			if hint := apexOwnershipHint(ep, all); hint != "" {
				args = append(args, "hint", hint)
			}
			p.log.Warn("skipping change: no Hostinger zone found for this name", args...)
			p.metrics.ObserveSkipped("no_zone")
			return nil
		}
		zc := byZone[zone]
		if zc == nil {
			zc = &zoneChanges{}
			byZone[zone] = zc
		}
		return zc
	}
	for _, ep := range changes.Delete {
		if zc := get(ep); zc != nil {
			zc.delete = append(zc.delete, ep)
		}
	}
	for _, pair := range pairUpdates(changes.UpdateOld, changes.UpdateNew) {
		if zc := get(pair.new); zc != nil {
			zc.update = append(zc.update, pair)
		}
	}
	for _, ep := range changes.Create {
		if zc := get(ep); zc != nil {
			zc.create = append(zc.create, ep)
		}
	}
	if len(byZone) == 0 {
		return nil
	}

	zones := make([]string, 0, len(byZone))
	for z := range byZone {
		zones = append(zones, z)
	}
	slices.Sort(zones)
	states, err := p.readZones(ctx, zones)
	if err != nil {
		return err
	}
	works := make([]*zoneWork, 0, len(states))
	read := map[string]bool{}
	for _, zs := range states {
		read[zs.name] = true
		w := &zoneWork{zone: zs.name, zc: byZone[zs.name], sets: map[endpointKey]*hostinger.RRset{}}
		for i := range zs.sets {
			s := zs.sets[i]
			w.sets[endpointKey{name: recordFQDN(s.Name, zs.name), recordType: s.Type}] = &s
		}
		works = append(works, w)
	}
	for _, z := range zones {
		if !read[z] {
			errs = append(errs, fmt.Errorf("zone %s is not in this Hostinger account; its changes were skipped", z))
		}
	}

	// 1. Delete records.
	failedDeletes, e := p.runPhase(ctx, works, func(w *zoneWork) []change {
		var out []change
		for _, ep := range w.zc.delete {
			if !isOwnershipRecord(ep) {
				out = append(out, change{action: "delete", ep: ep})
			}
		}
		return out
	})
	errs = append(errs, e...)

	// 2. Delete ownership records, unless their record is still there.
	_, e = p.runPhase(ctx, works, func(w *zoneWork) []change {
		var out []change
		for _, ep := range w.zc.delete {
			if !isOwnershipRecord(ep) {
				continue
			}
			if owned := ownedRecord(ep, failedDeletes); owned != nil {
				p.log.Warn("keeping ownership record because the record it owns could not be deleted",
					"ownership", ep.DNSName, "record", describe(owned))
				p.metrics.ObserveSkipped("ownership_kept")
				continue
			}
			out = append(out, change{action: "delete", ep: ep})
		}
		return out
	})
	errs = append(errs, e...)

	// 3. Update records.
	_, e = p.runPhase(ctx, works, func(w *zoneWork) []change {
		out := make([]change, 0, len(w.zc.update))
		for _, pair := range w.zc.update {
			out = append(out, change{action: "update", ep: pair.new, old: pair.old})
		}
		return out
	})
	errs = append(errs, e...)

	// 4. Create ownership records.
	failedOwners, e := p.runPhase(ctx, works, func(w *zoneWork) []change {
		var out []change
		for _, ep := range w.zc.create {
			if isOwnershipRecord(ep) {
				out = append(out, change{action: "create", ep: ep})
			}
		}
		return out
	})
	errs = append(errs, e...)

	// 5. Create records, unless their ownership record is missing.
	_, e = p.runPhase(ctx, works, func(w *zoneWork) []change {
		var out []change
		for _, ep := range w.zc.create {
			if isOwnershipRecord(ep) {
				continue
			}
			if owner := ownershipOf(ep, failedOwners); owner != nil {
				p.metrics.ObserveSkipped("owner_missing")
				errs = append(errs, fmt.Errorf("not creating %s: its ownership record %s could not be created", describe(ep), owner.DNSName))
				continue
			}
			out = append(out, change{action: "create", ep: ep})
		}
		return out
	})
	errs = append(errs, e...)

	if err := errors.Join(errs...); err != nil {
		return err
	}
	p.metrics.MarkSuccess()
	return nil
}

// runPhase plans the changes pick returns for every zone, sends them with as
// few calls as possible, and returns the endpoints that failed.
func (p *Provider) runPhase(ctx context.Context, works []*zoneWork, pick func(*zoneWork) []change) ([]*endpoint.Endpoint, []error) {
	var failed []*endpoint.Endpoint
	var errs []error
	for _, w := range works {
		changes := pick(w)
		if len(changes) == 0 {
			continue
		}
		ops, f, e := p.planOps(w, changes)
		failed, errs = append(failed, f...), append(errs, e...)
		f, e = p.sendOps(ctx, w, ops)
		failed, errs = append(failed, f...), append(errs, e...)
	}
	return failed, errs
}

// planOps turns changes into writes. Two changes to the same record set in
// one phase become one write.
func (p *Provider) planOps(w *zoneWork, changes []change) ([]*op, []*endpoint.Endpoint, []error) {
	var failed []*endpoint.Endpoint
	var errs []error
	var ops []*op
	byKey := map[endpointKey]*op{}
	current := func(k endpointKey) *hostinger.RRset {
		if o := byKey[k]; o != nil {
			return o.set
		}
		return w.sets[k]
	}
	for _, c := range changes {
		k := keyOf(c.ep)
		if err := checkName(k.name, w.zone); err != nil {
			if strings.Contains(k.name, ".*.") || strings.HasSuffix(k.name, ".*") {
				err = fmt.Errorf("%w (start external-dns with --txt-wildcard-replacement=wildcard)", err)
			}
			failed, errs = append(failed, c.ep), append(errs, fmt.Errorf("%s %s: %w", c.action, describe(c.ep), err))
			p.metrics.ObserveChange(c.action, err)
			continue
		}
		var set *hostinger.RRset
		var changed bool
		var err error
		switch c.action {
		case "delete":
			set, changed, err = p.planDelete(w.zone, k, c.ep, current(k))
		case "update":
			set, changed, err = p.planUpdate(w.zone, k, c.old, c.ep, current(k))
		default:
			set, changed, err = p.planCreate(w.zone, k, c.ep, current(k))
		}
		if err != nil {
			failed, errs = append(failed, c.ep), append(errs, fmt.Errorf("%s %s in zone %s: %w", c.action, describe(c.ep), w.zone, err))
			p.metrics.ObserveChange(c.action, err)
			p.log.Error("cannot apply change", "zone", w.zone, "action", c.action, "record", describe(c.ep), "error", err)
			continue
		}
		if !changed {
			p.log.Debug("nothing to do, Hostinger already matches", "zone", w.zone, "action", c.action, "record", describe(c.ep))
			continue
		}
		if o := byKey[k]; o != nil {
			o.set, o.eps = set, append(o.eps, c.ep)
			continue
		}
		o := &op{key: k, set: set, action: c.action, eps: []*endpoint.Endpoint{c.ep}}
		byKey[k] = o
		ops = append(ops, o)
	}
	return ops, failed, errs
}

// planDelete removes the endpoint's targets from the record set. The other
// values stay; if none are left, the set is deleted.
func (p *Provider) planDelete(zone string, k endpointKey, ep *endpoint.Endpoint, cur *hostinger.RRset) (*hostinger.RRset, bool, error) {
	if cur == nil {
		return nil, false, nil // already gone
	}
	if err := checkEditable(cur); err != nil {
		return nil, false, err
	}
	gone := map[string]bool{}
	for _, t := range ep.Targets {
		gone[canonicalTarget(k.recordType, t)] = true
	}
	var keep []hostinger.Record
	for _, r := range cur.Records {
		if t, _ := recordTarget(k.recordType, r.Content); !gone[t] {
			keep = append(keep, hostinger.Record{Content: r.Content})
		}
	}
	if len(keep) == len(cur.Records) {
		return cur, false, nil // none of the targets is there
	}
	if len(keep) == 0 {
		return nil, true, nil
	}
	return &hostinger.RRset{Name: relativeName(k.name, zone), Type: k.recordType, TTL: clampTTL(cur.TTL), Records: keep}, true, nil
}

// planUpdate makes the record set hold the new targets. Values that were
// neither in the old nor in the new endpoint were added by hand since the
// last read; they are kept (except for a CNAME, which has only one value).
func (p *Provider) planUpdate(zone string, k endpointKey, old, nw *endpoint.Endpoint, cur *hostinger.RRset) (*hostinger.RRset, bool, error) {
	want, err := contents(k, nw)
	if err != nil {
		return nil, false, err
	}
	ttl := p.ttlOf(nw)
	if cur == nil {
		return &hostinger.RRset{Name: relativeName(k.name, zone), Type: k.recordType, TTL: ttl, Records: want}, true, nil
	}
	if err := checkEditable(cur); err != nil {
		return nil, false, err
	}
	if k.recordType != hostinger.TypeCNAME {
		known := map[string]bool{}
		for _, t := range old.Targets {
			known[canonicalTarget(k.recordType, t)] = true
		}
		for _, t := range nw.Targets {
			known[canonicalTarget(k.recordType, t)] = true
		}
		for _, r := range cur.Records {
			if t, _ := recordTarget(k.recordType, r.Content); !known[t] {
				want = append(want, hostinger.Record{Content: r.Content})
			}
		}
	}
	if sameValues(k.recordType, want, cur.Records) && ttl == cur.TTL {
		return cur, false, nil
	}
	return &hostinger.RRset{Name: relativeName(k.name, zone), Type: k.recordType, TTL: ttl, Records: want}, true, nil
}

// planCreate adds the endpoint's targets to the record set. A set that
// already holds them is left alone, so a repeated create is safe. Values
// already in the set are kept: a create never removes anything.
func (p *Provider) planCreate(zone string, k endpointKey, ep *endpoint.Endpoint, cur *hostinger.RRset) (*hostinger.RRset, bool, error) {
	want, err := contents(k, ep)
	if err != nil {
		return nil, false, err
	}
	ttl := p.ttlOf(ep)
	if cur == nil {
		return &hostinger.RRset{Name: relativeName(k.name, zone), Type: k.recordType, TTL: ttl, Records: want}, true, nil
	}
	if err := checkEditable(cur); err != nil {
		return nil, false, err
	}
	have := map[string]bool{}
	merged := make([]hostinger.Record, 0, len(cur.Records)+len(want))
	for _, r := range cur.Records {
		t, _ := recordTarget(k.recordType, r.Content)
		have[t] = true
		merged = append(merged, hostinger.Record{Content: r.Content})
	}
	added := false
	for _, r := range want {
		if t, _ := recordTarget(k.recordType, r.Content); !have[t] {
			merged = append(merged, r)
			added = true
		}
	}
	if !added {
		return cur, false, nil
	}
	if k.recordType == hostinger.TypeCNAME {
		return nil, false, fmt.Errorf("a CNAME with another target already exists and external-dns does not own it; delete it by hand to let external-dns manage this name")
	}
	return &hostinger.RRset{Name: relativeName(k.name, zone), Type: k.recordType, TTL: ttl, Records: merged}, true, nil
}

// contents turns the targets of ep into Hostinger values.
func contents(k endpointKey, ep *endpoint.Endpoint) ([]hostinger.Record, error) {
	out := make([]hostinger.Record, 0, len(ep.Targets))
	seen := map[string]bool{}
	for _, t := range ep.Targets {
		c, err := contentFor(k.name, k.recordType, t)
		if err != nil {
			return nil, err
		}
		canon, _ := recordTarget(k.recordType, c)
		if !seen[canon] {
			seen[canon] = true
			out = append(out, hostinger.Record{Content: c})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no targets")
	}
	if k.recordType == hostinger.TypeCNAME && len(out) > 1 {
		return nil, errors.New("a CNAME can only have one target")
	}
	return out, nil
}

// checkEditable refuses to rewrite a record set that holds values turned
// off in hPanel. The API cannot write the "disabled" flag, so a rewrite
// would turn them on again or drop them.
func checkEditable(cur *hostinger.RRset) error {
	for _, r := range cur.Records {
		if r.IsDisabled {
			return fmt.Errorf("the record set %s %s has values that are disabled in hPanel; the webhook will not change it. Enable or remove them in hPanel", cur.Name, cur.Type)
		}
	}
	return nil
}

func sameValues(recordType string, a, b []hostinger.Record) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, r := range a {
		t, _ := recordTarget(recordType, r.Content)
		set[t] = true
	}
	for _, r := range b {
		if t, _ := recordTarget(recordType, r.Content); !set[t] {
			return false
		}
	}
	return true
}

// sendOps writes the planned ops of one zone: first one DELETE for every set
// that goes away, then one PUT for every set that changes. It returns the
// endpoints whose writes failed.
func (p *Provider) sendOps(ctx context.Context, w *zoneWork, ops []*op) ([]*endpoint.Endpoint, []error) {
	var dels, puts []*op
	for _, o := range ops {
		if o.set == nil {
			dels = append(dels, o)
		} else {
			puts = append(puts, o)
		}
	}
	var failed []*endpoint.Endpoint
	var errs []error
	for _, batch := range []struct {
		ops  []*op
		send func([]*op) error
	}{
		{dels, func(b []*op) error { return p.sendDelete(ctx, w.zone, b) }},
		{puts, func(b []*op) error { return p.sendPut(ctx, w.zone, b) }},
	} {
		if len(batch.ops) == 0 {
			continue
		}
		results := bisect(batch.ops, batch.send)
		for _, o := range batch.ops {
			err := results[o]
			p.metrics.ObserveChange(o.action, err)
			log := p.log.With("zone", w.zone, "action", o.action, "name", o.key.name, "type", o.key.recordType)
			if err != nil {
				log.Error("failed to change record set", "change", o.describe(w.zone), "error", err)
				failed = append(failed, o.eps...)
				errs = append(errs, fmt.Errorf("%s %s: %w", o.action, o.describe(w.zone), err))
				continue
			}
			switch {
			case p.dryRun:
			case o.set == nil:
				log.Info("deleted record set")
			default:
				log.Info("wrote record set", "ttl", o.set.TTL, "values", recordValues(o.set))
			}
			if o.set == nil {
				delete(w.sets, o.key)
			} else {
				w.sets[o.key] = o.set
			}
		}
	}
	return failed, errs
}

// bisect sends ops as one batch. Hostinger refuses a whole batch when one
// record set in it is invalid (HTTP 422), so a refused batch is split in
// halves and each half is tried again, until every bad set is found on its
// own. Temporary errors are not split: the whole batch fails and
// external-dns tries again on its next loop.
func bisect(ops []*op, send func([]*op) error) map[*op]error {
	out := map[*op]error{}
	err := send(ops)
	if err == nil {
		return out
	}
	if len(ops) == 1 || !hostinger.IsValidation(err) {
		for _, o := range ops {
			out[o] = err
		}
		return out
	}
	mid := len(ops) / 2
	for _, half := range [][]*op{ops[:mid], ops[mid:]} {
		for o, e := range bisect(half, send) {
			out[o] = e
		}
	}
	return out
}

func (p *Provider) sendDelete(ctx context.Context, zone string, ops []*op) error {
	filters := make([]hostinger.Filter, 0, len(ops))
	for _, o := range ops {
		filters = append(filters, hostinger.Filter{Name: relativeName(o.key.name, zone), Type: o.key.recordType})
	}
	if p.dryRun {
		p.log.Info("[dry run] would delete record sets", "zone", zone, "filters", filters)
		return nil
	}
	return p.api.DeleteRRsets(ctx, zone, filters)
}

func (p *Provider) sendPut(ctx context.Context, zone string, ops []*op) error {
	sets := make([]hostinger.RRset, 0, len(ops))
	for _, o := range ops {
		sets = append(sets, *o.set)
	}
	if p.dryRun {
		// Ask Hostinger whether the write would work, without writing.
		err := p.api.ValidateZone(ctx, zone, sets)
		for _, o := range ops {
			p.log.Info("[dry run] would write record set", "zone", zone, "name", o.key.name, "type", o.key.recordType,
				"ttl", o.set.TTL, "values", recordValues(o.set), "valid", err == nil)
		}
		return err
	}
	return p.api.UpdateZone(ctx, zone, sets)
}

func recordValues(s *hostinger.RRset) []string {
	out := make([]string, 0, len(s.Records))
	for _, r := range s.Records {
		out = append(out, r.Content)
	}
	return out
}

// pairUpdates matches UpdateOld and UpdateNew entries by name and type.
// external-dns keeps them in the same order, but matching by key is safer.
func pairUpdates(oldEps, newEps []*endpoint.Endpoint) []updatePair {
	type k struct {
		key endpointKey
		set string
	}
	olds := map[k]*endpoint.Endpoint{}
	for _, ep := range oldEps {
		olds[k{keyOf(ep), ep.SetIdentifier}] = ep
	}
	pairs := make([]updatePair, 0, len(newEps))
	for _, ep := range newEps {
		old := olds[k{keyOf(ep), ep.SetIdentifier}]
		if old == nil {
			old = &endpoint.Endpoint{DNSName: ep.DNSName, RecordType: ep.RecordType}
		}
		pairs = append(pairs, updatePair{old: old, new: ep})
	}
	return pairs
}

// apexOwnershipHint explains the most common reason a TXT name has no zone:
// the TXT registry turned the apex "example.com" into "a-example.com".
func apexOwnershipHint(ep *endpoint.Endpoint, zones []string) string {
	if ep.RecordType != hostinger.TypeTXT {
		return ""
	}
	name := normalizeName(ep.DNSName)
	for _, z := range zones {
		if strings.HasSuffix(name, "-"+z) {
			return "this looks like the ownership record of the zone apex " + z +
				". Start external-dns with --txt-prefix=extdns-%{record_type}. (ending in a dot) so it lands inside the zone"
		}
	}
	return ""
}

// isOwnershipRecord reports whether ep is a TXT record that the external-dns
// TXT registry uses to remember who owns a record.
func isOwnershipRecord(ep *endpoint.Endpoint) bool {
	if ep.RecordType != hostinger.TypeTXT {
		return false
	}
	for _, t := range ep.Targets {
		if strings.Contains(t, "heritage=external-dns") {
			return true
		}
	}
	return false
}

// ownedRecord returns the record in records that the ownership record txt
// belongs to, or nil.
func ownedRecord(txt *endpoint.Endpoint, records []*endpoint.Endpoint) *endpoint.Endpoint {
	for _, r := range records {
		if ownershipMatches(txt, r) {
			return r
		}
	}
	return nil
}

// ownershipOf returns the ownership record in owners that belongs to rec, or nil.
func ownershipOf(rec *endpoint.Endpoint, owners []*endpoint.Endpoint) *endpoint.Endpoint {
	for _, o := range owners {
		if ownershipMatches(o, rec) {
			return o
		}
	}
	return nil
}

// ownershipMatches guesses whether the ownership TXT record txt belongs to
// rec. The TXT registry names ownership records like
// extdns-a.www.example.com (a prefix that ends in a dot) or
// a-www.example.com (the default), so the TXT name must end in the record
// name. A wildcard "*.apps" becomes "wildcard.apps" with
// --txt-wildcard-replacement=wildcard, so the "*" is dropped before
// comparing. When both carry the Kubernetes resource that created them,
// that must match too. Guessing a little too often is safe: it only keeps
// a spare TXT record, or delays a create by one loop.
func ownershipMatches(txt, rec *endpoint.Endpoint) bool {
	tn := normalizeName(txt.DNSName)
	rn := strings.TrimPrefix(normalizeName(rec.DNSName), "*.")
	if !strings.HasSuffix(tn, "."+rn) && !strings.HasSuffix(tn, "-"+rn) {
		return false
	}
	tr, rr := ownershipResource(txt), rec.Labels[endpoint.ResourceLabelKey]
	if tr != "" && rr != "" {
		return tr == rr
	}
	return true
}

// ownershipResource reads the external-dns/resource label from the value of
// an ownership TXT record. It returns "" if the value is encrypted or has no
// such label.
func ownershipResource(txt *endpoint.Endpoint) string {
	key := "external-dns/" + endpoint.ResourceLabelKey + "="
	for _, t := range txt.Targets {
		for part := range strings.SplitSeq(unquote(strings.TrimSpace(t)), ",") {
			if v, ok := strings.CutPrefix(part, key); ok {
				return v
			}
		}
	}
	return ""
}
