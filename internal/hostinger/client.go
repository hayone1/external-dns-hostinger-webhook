// Package hostinger is a small client for the DNS part of the Hostinger API.
//
// It only covers what the webhook needs: the domain list, reading a zone,
// writing and deleting record sets, and validating a write. It handles the
// Bearer token, the rate limit, retries and Hostinger's error format.
//
// On purpose it has no way to reset a zone or restore a snapshot. Those calls
// can wipe a whole zone and must stay a human decision.
package hostinger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hayone1/external-dns-hostinger-webhook/internal/metrics"
)

const (
	// DefaultAPIURL is the address of the Hostinger API.
	DefaultAPIURL = "https://developers.hostinger.com"
	// maxResponseBytes caps how much of a response body is read.
	maxResponseBytes = 32 << 20
	// maxBackoff caps the wait between retries. Hostinger's rate limit
	// window is 60 seconds, so waiting longer never helps.
	maxBackoff = 60 * time.Second
)

// Options configures a Client.
type Options struct {
	APIURL         string
	Token          string
	RequestTimeout time.Duration
	// RateLimit is the most requests per second the client sends on
	// average. Hostinger allows 90 requests per minute per user, shared by
	// every token and tool of that user.
	RateLimit float64
	// Burst is how many requests may go out at once before RateLimit
	// applies.
	Burst      int
	MaxRetries int
	UserAgent  string
	Logger     *slog.Logger
	Metrics    *metrics.Metrics
	// HTTPClient is used for every call. Tests use it to inject a fake
	// server. If nil, a client with RequestTimeout is created.
	HTTPClient *http.Client
	// BaseBackoff is the first retry delay. It defaults to 1s.
	BaseBackoff time.Duration
}

// Client talks to the Hostinger API. It is safe for concurrent use.
type Client struct {
	base        string
	token       string
	http        *http.Client
	limiter     *rate.Limiter
	maxRetries  int
	userAgent   string
	log         *slog.Logger
	metrics     *metrics.Metrics
	baseBackoff time.Duration
	sleep       func(ctx context.Context, d time.Duration) error
	now         func() time.Time

	// pauseUntil is set when Hostinger says no requests are left in the
	// current window. Every request waits until then.
	mu         sync.Mutex
	pauseUntil time.Time
}

// New creates a Client.
func New(o Options) (*Client, error) {
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	base := strings.TrimRight(o.APIURL, "/")
	if u, err := url.Parse(base); err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("hostinger: invalid API URL %q", o.APIURL)
	}
	if strings.TrimSpace(o.Token) == "" {
		return nil, errors.New("hostinger: an API token is required")
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 30 * time.Second
	}
	if o.RateLimit <= 0 {
		o.RateLimit = 1
	}
	if o.Burst <= 0 {
		o.Burst = 5
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: o.RequestTimeout}
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = time.Second
	}
	return &Client{
		base:        base,
		token:       strings.TrimSpace(o.Token),
		http:        o.HTTPClient,
		limiter:     rate.NewLimiter(rate.Limit(o.RateLimit), o.Burst),
		maxRetries:  o.MaxRetries,
		userAgent:   o.UserAgent,
		log:         o.Logger,
		metrics:     o.Metrics,
		baseBackoff: o.BaseBackoff,
		sleep:       sleepCtx,
		now:         time.Now,
	}, nil
}

// CheckToken makes one cheap read call, which proves the token works.
func (c *Client) CheckToken(ctx context.Context) error {
	_, err := c.ListDomains(ctx)
	return err
}

// ListDomains returns the domain portfolio of the account. Hostinger has
// no "list DNS zones" call, so the portfolio is the list of zone candidates.
func (c *Client) ListDomains(ctx context.Context) ([]Domain, error) {
	body, err := c.do(ctx, "listDomains", http.MethodGet, "/api/domains/v1/portfolio", nil)
	if err != nil {
		return nil, err
	}
	var out []Domain
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("hostinger listDomains: decode response: %w", err)
	}
	return out, nil
}

// GetDomain returns the details of one domain, including its name servers.
func (c *Client) GetDomain(ctx context.Context, domain string) (DomainDetails, error) {
	body, err := c.do(ctx, "getDomain", http.MethodGet, "/api/domains/v1/portfolio/"+url.PathEscape(domain), nil)
	if err != nil {
		return DomainDetails{}, err
	}
	var out DomainDetails
	if err := json.Unmarshal(body, &out); err != nil {
		return DomainDetails{}, fmt.Errorf("hostinger getDomain: decode response: %w", err)
	}
	return out, nil
}

// GetZone returns every record set of a zone. The answer is never paged.
// NS and SOA are not part of it: Hostinger manages them out of sight.
func (c *Client) GetZone(ctx context.Context, domain string) ([]RRset, error) {
	body, err := c.do(ctx, "getZone", http.MethodGet, zonePath(domain), nil)
	if err != nil {
		return nil, err
	}
	var out []RRset
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("hostinger getZone: decode response: %w", err)
	}
	return out, nil
}

// UpdateZone writes record sets with overwrite=true: each set with the same
// name and type is replaced as a whole, other sets are created. Sets that
// are not named are left alone. The call is all or nothing: if one set is
// invalid, Hostinger answers HTTP 422 and changes nothing.
func (c *Client) UpdateZone(ctx context.Context, domain string, sets []RRset) error {
	if err := checkWritable(sets); err != nil {
		return err
	}
	_, err := c.do(ctx, "updateZone", http.MethodPut, zonePath(domain), updateRequest{Overwrite: true, Zone: sets})
	return err
}

// ValidateZone asks Hostinger whether UpdateZone with the same sets would
// work, without changing anything.
func (c *Client) ValidateZone(ctx context.Context, domain string, sets []RRset) error {
	if err := checkWritable(sets); err != nil {
		return err
	}
	_, err := c.do(ctx, "validateZone", http.MethodPost, zonePath(domain)+"/validate", updateRequest{Overwrite: true, Zone: sets})
	return err
}

// DeleteRRsets deletes whole record sets by name and type. A filter that
// matches nothing is not an error.
func (c *Client) DeleteRRsets(ctx context.Context, domain string, filters []Filter) error {
	if len(filters) == 0 {
		return nil
	}
	for _, f := range filters {
		if ProtectedTypes[f.Type] {
			return fmt.Errorf("hostinger: refusing to delete %s records, Hostinger manages them", f.Type)
		}
	}
	_, err := c.do(ctx, "deleteRRsets", http.MethodDelete, zonePath(domain), deleteRequest{Filters: filters})
	return err
}

func zonePath(domain string) string {
	return "/api/dns/v1/zones/" + url.PathEscape(domain)
}

func checkWritable(sets []RRset) error {
	if len(sets) == 0 {
		return errors.New("hostinger: nothing to write")
	}
	for _, s := range sets {
		if ProtectedTypes[s.Type] {
			return fmt.Errorf("hostinger: refusing to write %s records, Hostinger manages them", s.Type)
		}
		if len(s.Records) == 0 {
			return fmt.Errorf("hostinger: record set %s %s has no values; delete it instead", s.Name, s.Type)
		}
	}
	return nil
}

// do sends one API request with retries and returns the response body.
func (c *Client) do(ctx context.Context, op, method, path string, in any) ([]byte, error) {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return nil, fmt.Errorf("hostinger %s: encode request: %w", op, err)
		}
	}
	// path is already escaped, so join strings instead of using url.URL.Path.
	rawURL := c.base + path

	for attempt := 0; ; attempt++ {
		body, retryAfter, err := c.once(ctx, op, method, rawURL, payload)
		if err == nil {
			return body, nil
		}
		if !IsTemporary(err) || ctx.Err() != nil || attempt >= c.maxRetries {
			return nil, err
		}
		wait := c.backoff(attempt, retryAfter)
		reason := "network"
		if apiErr, ok := errors.AsType[*APIError](err); ok {
			reason = strconv.Itoa(apiErr.StatusCode)
		}
		c.metrics.ObserveRetry(op, reason)
		c.log.Debug("retrying Hostinger API request", "operation", op, "attempt", attempt+1, "reason", reason, "wait", wait, "error", err)
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// once sends a single request. It returns the body on success, or an error
// plus the server's suggested wait time.
func (c *Client) once(ctx context.Context, op, method, rawURL string, payload []byte) ([]byte, time.Duration, error) {
	if err := c.waitForBudget(ctx); err != nil {
		return nil, 0, err
	}

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("hostinger %s: build request: %w", op, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.metrics.ObserveRequest(op, 0, time.Since(start))
		return nil, 0, fmt.Errorf("hostinger %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	c.metrics.ObserveRequest(op, resp.StatusCode, time.Since(start))
	c.noteRateLimit(resp.Header)

	c.log.Debug("Hostinger API call", "operation", op, "method", method, "status", resp.StatusCode,
		"duration", time.Since(start))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if readErr != nil {
			return nil, 0, fmt.Errorf("hostinger %s: read response: %w", op, readErr)
		}
		return data, 0, nil
	}
	apiErr := parseError(op, resp.StatusCode, data)
	if resp.StatusCode == http.StatusUnauthorized {
		c.metrics.ObserveAuthFailure()
		return nil, 0, fmt.Errorf("%w: %w", ErrInvalidToken, apiErr)
	}
	if apiErr.CorrelationID != "" {
		c.log.Debug("Hostinger API error", "operation", op, "status", resp.StatusCode, "code", apiErr.Code,
			"correlationID", apiErr.CorrelationID)
	}
	return nil, retryAfter(resp.Header, c.now()), apiErr
}

// waitForBudget waits for the local rate limiter and for any pause that
// Hostinger asked for.
func (c *Client) waitForBudget(ctx context.Context) error {
	c.mu.Lock()
	until := c.pauseUntil
	c.mu.Unlock()
	if d := until.Sub(c.now()); d > 0 {
		c.log.Debug("Hostinger rate limit used up, pausing", "wait", d)
		if err := c.sleep(ctx, d); err != nil {
			return err
		}
	}
	return c.limiter.Wait(ctx)
}

// rateLimitRE reads the IETF draft header Hostinger sends:
//
//	RateLimit: "api";r=89;t=60
//
// r is the number of requests left, t the seconds until the window resets.
var rateLimitRE = regexp.MustCompile(`(?:^|;)\s*([rt])=(\d+)`)

// noteRateLimit stores what is left of the rate limit window. When nothing
// is left, the client pauses until the window resets instead of getting
// HTTP 429 (repeated 429s can get the caller's IP address blocked).
func (c *Client) noteRateLimit(h http.Header) {
	remaining, reset := -1, -1
	if v := h.Get("RateLimit"); v != "" {
		for _, m := range rateLimitRE.FindAllStringSubmatch(v, -1) {
			n, _ := strconv.Atoi(m[2])
			if m[1] == "r" {
				remaining = n
			} else {
				reset = n
			}
		}
	}
	if remaining < 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Remaining"))); err == nil {
			remaining = n
		}
	}
	if remaining < 0 {
		return
	}
	c.metrics.SetRateLimitRemaining(float64(remaining))
	if remaining == 0 {
		if reset <= 0 || reset > 60 {
			reset = 60
		}
		c.mu.Lock()
		c.pauseUntil = c.now().Add(time.Duration(reset) * time.Second)
		c.mu.Unlock()
	}
}

// retryAfter reads the server's suggested wait from Retry-After. It holds
// either a number of seconds or an HTTP date.
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// backoff returns an exponential delay with full jitter, or the server hint
// if that is longer.
func (c *Client) backoff(attempt int, hint time.Duration) time.Duration {
	d := c.baseBackoff << min(attempt, 10)
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	d = d/2 + rand.N(d/2+1) //nolint:gosec // jitter for retry timing, not security
	if hint > d {
		d = min(hint, maxBackoff)
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
