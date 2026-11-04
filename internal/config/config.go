// Package config reads the webhook configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Default values used when an environment variable is not set.
const (
	DefaultAPIURL         = "https://developers.hostinger.com"
	DefaultWebhookHost    = "localhost"
	DefaultWebhookPort    = 8888
	DefaultHealthHost     = "0.0.0.0"
	DefaultHealthPort     = 8080
	DefaultTTL            = 3600
	MinTTL                = 60
	MaxTTL                = 86400
	DefaultZoneCacheTTL   = 5 * time.Minute
	DefaultRequestTimeout = 30 * time.Second
	DefaultRateLimit      = 1.0
	DefaultRateBurst      = 5
	DefaultMaxRetries     = 5
	DefaultLogLevel       = "info"
	DefaultLogFormat      = "text"
)

// Config holds every setting the webhook needs.
type Config struct {
	APIToken string
	APIURL   string

	DomainFilter        []string
	ExcludeDomainFilter []string
	RegexDomainFilter   *regexp.Regexp
	RegexDomainExclude  *regexp.Regexp

	DryRun         bool
	DefaultTTL     int64
	RequestTimeout time.Duration
	RateLimit      float64
	RateBurst      int
	MaxRetries     int
	ZoneCacheTTL   time.Duration

	WebhookHost string
	WebhookPort int
	HealthHost  string
	HealthPort  int

	LogLevel  string
	LogFormat string
}

// WebhookAddress is the listen address of the provider API.
func (c *Config) WebhookAddress() string {
	return net.JoinHostPort(c.WebhookHost, strconv.Itoa(c.WebhookPort))
}

// HealthAddress is the listen address of the health and metrics server.
func (c *Config) HealthAddress() string {
	return net.JoinHostPort(c.HealthHost, strconv.Itoa(c.HealthPort))
}

// Load reads the configuration from the process environment.
func Load() (*Config, error) {
	return LoadFrom(os.LookupEnv, os.ReadFile)
}

// LookupFunc matches os.LookupEnv.
type LookupFunc func(key string) (string, bool)

// ReadFileFunc matches os.ReadFile.
type ReadFileFunc func(name string) ([]byte, error)

// LoadFrom reads the configuration using the given lookup functions.
// It exists so tests do not need to touch the real environment.
func LoadFrom(lookup LookupFunc, readFile ReadFileFunc) (*Config, error) {
	r := reader{lookup: lookup, readFile: readFile}

	c := &Config{
		APIToken: r.secret("HOSTINGER_API_TOKEN"),
		APIURL:   r.str("HOSTINGER_API_URL", DefaultAPIURL),

		DomainFilter:        r.list("DOMAIN_FILTER"),
		ExcludeDomainFilter: r.list("EXCLUDE_DOMAIN_FILTER"),
		RegexDomainFilter:   r.regex("REGEX_DOMAIN_FILTER"),
		RegexDomainExclude:  r.regex("REGEX_DOMAIN_EXCLUSION"),

		DryRun:         r.boolean("DRY_RUN", false),
		DefaultTTL:     int64(r.integer("HOSTINGER_DEFAULT_TTL", DefaultTTL)),
		RequestTimeout: r.duration("HOSTINGER_REQUEST_TIMEOUT", DefaultRequestTimeout),
		RateLimit:      r.float("HOSTINGER_RATE_LIMIT", DefaultRateLimit),
		RateBurst:      r.integer("HOSTINGER_RATE_BURST", DefaultRateBurst),
		MaxRetries:     r.integer("HOSTINGER_MAX_RETRIES", DefaultMaxRetries),
		ZoneCacheTTL:   r.duration("HOSTINGER_ZONE_CACHE_TTL", DefaultZoneCacheTTL),

		WebhookHost: r.str("WEBHOOK_HOST", DefaultWebhookHost),
		WebhookPort: r.integer("WEBHOOK_PORT", DefaultWebhookPort),
		HealthHost:  r.str("HEALTH_HOST", DefaultHealthHost),
		HealthPort:  r.integer("HEALTH_PORT", DefaultHealthPort),

		LogLevel:  strings.ToLower(r.str("LOG_LEVEL", DefaultLogLevel)),
		LogFormat: strings.ToLower(r.str("LOG_FORMAT", DefaultLogFormat)),
	}

	if err := errors.Join(r.errs...); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	var errs []error
	if c.APIToken == "" {
		errs = append(errs, errors.New("HOSTINGER_API_TOKEN is required (set it directly or through HOSTINGER_API_TOKEN_FILE); "+
			"create one in hPanel under Profile > API"))
	} else if strings.ContainsAny(c.APIToken, " \t\r\n") {
		errs = append(errs, errors.New("HOSTINGER_API_TOKEN must not contain spaces or line breaks"))
	}
	if u, err := url.Parse(c.APIURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		errs = append(errs, fmt.Errorf("HOSTINGER_API_URL must be an absolute http(s) URL, got %q", c.APIURL))
	}
	if (len(c.DomainFilter) > 0 || len(c.ExcludeDomainFilter) > 0) && (c.RegexDomainFilter != nil || c.RegexDomainExclude != nil) {
		errs = append(errs, errors.New("use either DOMAIN_FILTER/EXCLUDE_DOMAIN_FILTER or REGEX_DOMAIN_FILTER/REGEX_DOMAIN_EXCLUSION, not both"))
	}
	if c.DefaultTTL < MinTTL || c.DefaultTTL > MaxTTL {
		errs = append(errs, fmt.Errorf("HOSTINGER_DEFAULT_TTL must be between %d and %d (what Hostinger accepts), got %d", MinTTL, MaxTTL, c.DefaultTTL))
	}
	if c.RequestTimeout <= 0 {
		errs = append(errs, fmt.Errorf("HOSTINGER_REQUEST_TIMEOUT must be positive, got %s", c.RequestTimeout))
	}
	if c.RateLimit <= 0 || c.RateLimit > 1.5 {
		errs = append(errs, fmt.Errorf("HOSTINGER_RATE_LIMIT must be above 0 and at most 1.5 requests per second (Hostinger allows 90 per minute), got %v", c.RateLimit))
	}
	if c.RateBurst < 1 || c.RateBurst > 30 {
		errs = append(errs, fmt.Errorf("HOSTINGER_RATE_BURST must be between 1 and 30, got %d", c.RateBurst))
	}
	if c.MaxRetries < 0 {
		errs = append(errs, fmt.Errorf("HOSTINGER_MAX_RETRIES must not be negative, got %d", c.MaxRetries))
	}
	if c.ZoneCacheTTL < 0 {
		errs = append(errs, fmt.Errorf("HOSTINGER_ZONE_CACHE_TTL must not be negative, got %s", c.ZoneCacheTTL))
	}
	for name, port := range map[string]int{"WEBHOOK_PORT": c.WebhookPort, "HEALTH_PORT": c.HealthPort} {
		if port < 1 || port > 65535 {
			errs = append(errs, fmt.Errorf("%s must be between 1 and 65535, got %d", name, port))
		}
	}
	if c.WebhookAddress() == c.HealthAddress() {
		errs = append(errs, errors.New("WEBHOOK and HEALTH servers cannot use the same host and port"))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("LOG_LEVEL must be one of debug, info, warn, error, got %q", c.LogLevel))
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be text or json, got %q", c.LogFormat))
	}
	return errors.Join(errs...)
}

type reader struct {
	lookup   LookupFunc
	readFile ReadFileFunc
	errs     []error
}

func (r *reader) raw(key string) (string, bool) {
	v, ok := r.lookup(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	return v, v != ""
}

func (r *reader) str(key, def string) string {
	if v, ok := r.raw(key); ok {
		return v
	}
	return def
}

// secret reads KEY, or the file named by KEY_FILE. Setting both is an error,
// because it is unclear which one the user meant.
func (r *reader) secret(key string) string {
	v, hasValue := r.raw(key)
	path, hasFile := r.raw(key + "_FILE")
	switch {
	case hasValue && hasFile:
		r.errs = append(r.errs, fmt.Errorf("set only one of %s and %s_FILE", key, key))
		return ""
	case hasFile:
		b, err := r.readFile(path)
		if err != nil {
			r.errs = append(r.errs, fmt.Errorf("%s_FILE: %w", key, err))
			return ""
		}
		return strings.TrimSpace(string(b))
	default:
		return v
	}
}

func (r *reader) list(key string) []string {
	v, ok := r.raw(key)
	if !ok {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (r *reader) regex(key string) *regexp.Regexp {
	v, ok := r.raw(key)
	if !ok {
		return nil
	}
	re, err := regexp.Compile(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s is not a valid regular expression: %w", key, err))
		return nil
	}
	return re
}

func (r *reader) boolean(key string, def bool) bool {
	v, ok := r.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be true or false, got %q", key, v))
		return def
	}
	return b
}

func (r *reader) integer(key string, def int) int {
	v, ok := r.raw(key)
	if !ok {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be a whole number, got %q", key, v))
		return def
	}
	return i
}

func (r *reader) float(key string, def float64) float64 {
	v, ok := r.raw(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be a number, got %q", key, v))
		return def
	}
	return f
}

func (r *reader) duration(key string, def time.Duration) time.Duration {
	v, ok := r.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be a duration like 30s or 1m, got %q", key, v))
		return def
	}
	return d
}
