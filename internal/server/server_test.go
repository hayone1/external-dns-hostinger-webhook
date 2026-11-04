package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
	"sigs.k8s.io/external-dns/provider/webhook/api"

	"github.com/hayone1/external-dns-hostinger-webhook/internal/metrics"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestHealthAndReadiness(t *testing.T) {
	st := &Status{}
	h := HealthHandler(st, metrics.New("test").Registry)

	if code, _ := get(t, h, "/healthz"); code != 503 {
		t.Errorf("healthz before start = %d", code)
	}
	st.SetHealthy(true)
	if code, _ := get(t, h, "/healthz"); code != 200 {
		t.Errorf("healthz healthy = %d", code)
	}
	if code, _ := get(t, h, "/readyz"); code != 503 {
		t.Errorf("readyz before ready = %d", code)
	}
	st.SetReady(true)
	if code, _ := get(t, h, "/readyz"); code != 200 {
		t.Errorf("readyz ready = %d", code)
	}
	st.SetHealthy(false)
	if code, _ := get(t, h, "/readyz"); code != 503 {
		t.Errorf("readyz must fail when unhealthy, got %d", code)
	}
	code, body := get(t, h, "/metrics")
	if code != 200 || !strings.Contains(body, "hostinger_webhook_build_info") {
		t.Errorf("metrics = %d %q", code, body[:min(len(body), 200)])
	}
	if code, _ := get(t, h, "/nope"); code != 404 {
		t.Errorf("unknown path = %d", code)
	}
}

type stubProvider struct {
	provider.BaseProvider
	filter *endpoint.DomainFilter
}

func (s *stubProvider) Records(context.Context) ([]*endpoint.Endpoint, error) {
	return []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1")}, nil
}
func (s *stubProvider) ApplyChanges(context.Context, *plan.Changes) error { return nil }
func (s *stubProvider) GetDomainFilter() endpoint.DomainFilterInterface   { return s.filter }

func TestWebhookHandler(t *testing.T) {
	srv := httptest.NewServer(WebhookHandler(&stubProvider{filter: endpoint.NewDomainFilter([]string{"example.com"})}, nil))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("Accept", api.MediaTypeFormatAndVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.Header.Get("Content-Type") != api.MediaTypeFormatAndVersion || !strings.Contains(string(body), "example.com") {
		t.Fatalf("negotiate: %s %q", resp.Header.Get("Content-Type"), body)
	}

	resp, err = http.Get(srv.URL + "/records")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "www.example.com") {
		t.Fatalf("records: %d %q", resp.StatusCode, body)
	}

	resp, err = http.Post(srv.URL+"/records", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad body should be 400, got %d", resp.StatusCode)
	}

	resp, err = http.Post(srv.URL+"/records", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("apply should be 204, got %d", resp.StatusCode)
	}

	big := strings.NewReader(`[` + strings.Repeat(`{"dnsName":"x"},`, MaxBodySize/16+1) + `{}]`)
	resp, err = http.Post(srv.URL+"/adjustendpoints", "application/json", big)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body should be 413, got %d", resp.StatusCode)
	}
}

func TestServeShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := New("127.0.0.1:0", http.NotFoundHandler())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, started) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestServeBadAddress(t *testing.T) {
	if err := Serve(context.Background(), New("256.0.0.1:99999", http.NotFoundHandler()), nil); err == nil {
		t.Fatal("expected a listen error")
	}
}
