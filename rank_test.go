package main

import (
	"reflect"
	"testing"
	"time"
)

func TestOrderSoonestResetFirst(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	readings := map[string]reading{
		"late":  {reset: now.Add(96 * time.Hour), used: 0.1},
		"soon":  {reset: now.Add(20 * time.Hour), used: 0.6},
		"spent": {reset: now.Add(2 * time.Hour), used: 0.99},
	}
	got := order(readings, []string{"unknown", "late", "spent", "soon"}, 0.98)
	want := []string{"soon", "late", "spent", "unknown"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestObservedIgnoresPassedReset(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := authFile{}
	f.Quota.Signals = map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset":       "1699999999",
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.4",
	}
	if _, ok := observed(f, now); ok {
		t.Fatal("a passed reset still counted as current")
	}
	f.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = "1700003600"
	r, ok := observed(f, now)
	if !ok || r.used != 0.4 || r.reset.Unix() != 1700003600 {
		t.Fatalf("observed = %+v, %v", r, ok)
	}
}

func TestParseUsagePercent(t *testing.T) {
	r, ok := parseUsage([]byte(`{"seven_day":{"utilization":60,"resets_at":"2026-10-31T08:00:00Z"}}`))
	if !ok || r.used != 0.6 || r.reset.UTC().Hour() != 8 {
		t.Fatalf("parseUsage = %+v, %v", r, ok)
	}
	if _, ok := parseUsage([]byte(`{"five_hour":{}}`)); ok {
		t.Fatal("a body without seven_day parsed")
	}
}
