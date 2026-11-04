package metrics

import (
	"errors"
	"testing"
	"time"
)

func TestNilMetricsAreSafe(t *testing.T) {
	var m *Metrics
	m.ObserveRequest("x", 200, time.Second)
	m.ObserveRetry("x", "429")
	m.SetRateLimitRemaining(1)
	m.ObserveChange("create", nil)
	m.SetInventory(1, 2)
	m.MarkSuccess()
	m.ObserveAuthFailure()
	m.ObserveSkipped("x")
}

func TestMetricsRegister(t *testing.T) {
	m := New("v1")
	m.ObserveRequest("listZones", 200, time.Millisecond)
	m.ObserveChange("create", errors.New("x"))
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range families {
		found[f.GetName()] = true
	}
	for _, name := range []string{"hostinger_webhook_build_info", "hostinger_webhook_api_requests_total", "hostinger_webhook_record_changes_total", "go_goroutines"} {
		if !found[name] {
			t.Errorf("metric %s not registered", name)
		}
	}
}
