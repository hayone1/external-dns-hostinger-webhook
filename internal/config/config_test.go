package config

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) LookupFunc {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func noFiles(string) ([]byte, error) { return nil, os.ErrNotExist }

func baseEnv() map[string]string {
	return map[string]string{
		"HOSTINGER_API_TOKEN": "token-123",
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := LoadFrom(envOf(baseEnv()), noFiles)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.APIURL != DefaultAPIURL {
		t.Errorf("wrong URL: %s", c.APIURL)
	}
	if c.WebhookAddress() != "localhost:8888" {
		t.Errorf("webhook address = %s", c.WebhookAddress())
	}
	if c.HealthAddress() != "0.0.0.0:8080" {
		t.Errorf("health address = %s", c.HealthAddress())
	}
	if c.DefaultTTL != DefaultTTL || c.RateLimit != DefaultRateLimit || c.RateBurst != DefaultRateBurst ||
		c.MaxRetries != DefaultMaxRetries || c.ZoneCacheTTL != DefaultZoneCacheTTL {
		t.Errorf("wrong numeric defaults: %+v", c)
	}
	if c.DryRun {
		t.Error("dry run should default to false")
	}
	if c.LogLevel != "info" || c.LogFormat != "text" {
		t.Errorf("wrong log defaults: %s %s", c.LogLevel, c.LogFormat)
	}
}

func TestLoadOverrides(t *testing.T) {
	env := baseEnv()
	env["DOMAIN_FILTER"] = " example.com, ,example.org "
	env["EXCLUDE_DOMAIN_FILTER"] = "private.example.com"
	env["DRY_RUN"] = "TRUE"
	env["HOSTINGER_DEFAULT_TTL"] = "300"
	env["HOSTINGER_REQUEST_TIMEOUT"] = "5s"
	env["HOSTINGER_RATE_LIMIT"] = "0.5"
	env["HOSTINGER_RATE_BURST"] = "2"
	env["HOSTINGER_MAX_RETRIES"] = "0"
	env["HOSTINGER_ZONE_CACHE_TTL"] = "0s"
	env["WEBHOOK_HOST"] = "::1"
	env["WEBHOOK_PORT"] = "9999"
	env["LOG_LEVEL"] = "DEBUG"
	env["LOG_FORMAT"] = "json"

	c, err := LoadFrom(envOf(env), noFiles)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(c.DomainFilter, "|") != "example.com|example.org" {
		t.Errorf("domain filter = %v", c.DomainFilter)
	}
	if len(c.ExcludeDomainFilter) != 1 {
		t.Errorf("exclude filter = %v", c.ExcludeDomainFilter)
	}
	if !c.DryRun || c.DefaultTTL != 300 || c.RequestTimeout != 5*time.Second || c.RateLimit != 0.5 ||
		c.RateBurst != 2 || c.MaxRetries != 0 || c.ZoneCacheTTL != 0 {
		t.Errorf("overrides not applied: %+v", c)
	}
	if c.WebhookAddress() != "[::1]:9999" {
		t.Errorf("IPv6 address not bracketed: %s", c.WebhookAddress())
	}
	if c.LogLevel != "debug" {
		t.Errorf("log level not lower-cased: %s", c.LogLevel)
	}
}

func TestSecretFromFile(t *testing.T) {
	env := baseEnv()
	delete(env, "HOSTINGER_API_TOKEN")
	env["HOSTINGER_API_TOKEN_FILE"] = "/secrets/token"
	read := func(name string) ([]byte, error) {
		if name == "/secrets/token" {
			return []byte("from-file\n"), nil
		}
		return nil, os.ErrNotExist
	}
	c, err := LoadFrom(envOf(env), read)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.APIToken != "from-file" {
		t.Errorf("token = %q, want trimmed file content", c.APIToken)
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name string
		mod  func(map[string]string)
		want string
	}{
		{"missing token", func(e map[string]string) { delete(e, "HOSTINGER_API_TOKEN") }, "HOSTINGER_API_TOKEN is required"},
		{"blank token", func(e map[string]string) { e["HOSTINGER_API_TOKEN"] = "   " }, "HOSTINGER_API_TOKEN is required"},
		{"token with space", func(e map[string]string) { e["HOSTINGER_API_TOKEN"] = "abc def" }, "must not contain spaces"},
		{"value and file", func(e map[string]string) { e["HOSTINGER_API_TOKEN_FILE"] = "/x" }, "set only one of HOSTINGER_API_TOKEN"},
		{"missing file", func(e map[string]string) {
			delete(e, "HOSTINGER_API_TOKEN")
			e["HOSTINGER_API_TOKEN_FILE"] = "/missing"
		}, "HOSTINGER_API_TOKEN_FILE"},
		{"bad bool", func(e map[string]string) { e["DRY_RUN"] = "maybe" }, "DRY_RUN must be true or false"},
		{"bad int", func(e map[string]string) { e["WEBHOOK_PORT"] = "abc" }, "WEBHOOK_PORT must be a whole number"},
		{"port range", func(e map[string]string) { e["HEALTH_PORT"] = "70000" }, "HEALTH_PORT must be between"},
		{"same address", func(e map[string]string) {
			e["WEBHOOK_HOST"] = "0.0.0.0"
			e["WEBHOOK_PORT"] = "8080"
		}, "cannot use the same host and port"},
		{"bad regex", func(e map[string]string) { e["REGEX_DOMAIN_FILTER"] = "([" }, "not a valid regular expression"},
		{"mixed filters", func(e map[string]string) {
			e["DOMAIN_FILTER"] = "a.com"
			e["REGEX_DOMAIN_FILTER"] = ".*"
		}, "not both"},
		{"ttl too low", func(e map[string]string) { e["HOSTINGER_DEFAULT_TTL"] = "59" }, "HOSTINGER_DEFAULT_TTL must be between 60 and 86400"},
		{"ttl too high", func(e map[string]string) { e["HOSTINGER_DEFAULT_TTL"] = "86401" }, "HOSTINGER_DEFAULT_TTL"},
		{"bad url", func(e map[string]string) { e["HOSTINGER_API_URL"] = "developers.hostinger.com" }, "HOSTINGER_API_URL must be"},
		{"bad duration", func(e map[string]string) { e["HOSTINGER_REQUEST_TIMEOUT"] = "10" }, "duration"},
		{"negative timeout", func(e map[string]string) { e["HOSTINGER_REQUEST_TIMEOUT"] = "-1s" }, "must be positive"},
		{"zero rate", func(e map[string]string) { e["HOSTINGER_RATE_LIMIT"] = "0" }, "HOSTINGER_RATE_LIMIT"},
		{"rate above the API limit", func(e map[string]string) { e["HOSTINGER_RATE_LIMIT"] = "2" }, "at most 1.5"},
		{"bad float", func(e map[string]string) { e["HOSTINGER_RATE_LIMIT"] = "fast" }, "must be a number"},
		{"zero burst", func(e map[string]string) { e["HOSTINGER_RATE_BURST"] = "0" }, "HOSTINGER_RATE_BURST"},
		{"negative retries", func(e map[string]string) { e["HOSTINGER_MAX_RETRIES"] = "-1" }, "must not be negative"},
		{"negative cache", func(e map[string]string) { e["HOSTINGER_ZONE_CACHE_TTL"] = "-1m" }, "HOSTINGER_ZONE_CACHE_TTL"},
		{"bad log level", func(e map[string]string) { e["LOG_LEVEL"] = "trace" }, "LOG_LEVEL"},
		{"bad log format", func(e map[string]string) { e["LOG_FORMAT"] = "xml" }, "LOG_FORMAT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv()
			tt.mod(env)
			_, err := LoadFrom(envOf(env), noFiles)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestErrorsNeverLeakSecrets(t *testing.T) {
	env := baseEnv()
	env["HOSTINGER_API_TOKEN"] = "super secret-value"
	env["LOG_LEVEL"] = "nope"
	_, err := LoadFrom(envOf(env), noFiles)
	if err == nil || strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("error must exist and must not contain the secret: %v", err)
	}
	if errors.Unwrap(err) == nil && !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("unexpected error: %v", err)
	}
}
