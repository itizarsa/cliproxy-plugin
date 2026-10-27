package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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

func TestQuotaResponseBuckets(t *testing.T) {
	resp, err := quotaResponse([]byte(`{"five_hour": {"utilization": 25.0, "resets_at": "2026-10-27T12:00:00+00:00"},
		"seven_day": {"utilization": 75.0, "resets_at": "2026-10-31T17:00:00+00:00"}, "seven_day_opus": null}`))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Groups[0].Buckets
	want := []pluginapi.QuotaBucket{
		{Window: "5h", RemainingFraction: 0.75, ResetTime: "2026-10-27T12:00:00Z"},
		{Window: "7d", RemainingFraction: 0.25, ResetTime: "2026-10-31T17:00:00Z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buckets = %+v, want %+v", got, want)
	}
}
