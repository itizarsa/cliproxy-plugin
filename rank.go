package main

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// reading is what is known of one account's weekly window.
type reading struct {
	reset time.Time
	used  float64 // 0-1
}

// authFile is the part of GET /v0/management/auth-files this plugin reads.
type authFile struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	Email     string `json:"email"`
	Disabled  bool   `json:"disabled"`
	Quota     struct {
		Signals map[string]string `json:"signals"`
	} `json:"quota"`
}

// observed reads the weekly window from the rate-limit headers the hub last saw for the account.
// A reading whose reset has passed says nothing about the window now open.
func observed(f authFile, now time.Time) (reading, bool) {
	resetUnix, errReset := strconv.ParseInt(f.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"], 10, 64)
	used, errUsed := strconv.ParseFloat(f.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"], 64)
	if errReset != nil || errUsed != nil {
		return reading{}, false
	}
	r := reading{reset: time.Unix(resetUnix, 0), used: used}
	return r, r.reset.After(now)
}

// parseUsage reads Anthropic's /api/oauth/usage body, whose utilization is a percentage.
func parseUsage(body []byte) (reading, bool) {
	var usage struct {
		SevenDay *struct {
			Utilization float64   `json:"utilization"`
			ResetsAt    time.Time `json:"resets_at"`
		} `json:"seven_day"`
	}
	if json.Unmarshal(body, &usage) != nil || usage.SevenDay == nil || usage.SevenDay.ResetsAt.IsZero() {
		return reading{}, false
	}
	return reading{reset: usage.SevenDay.ResetsAt, used: usage.SevenDay.Utilization / 100}, true
}

// quotaResponse turns the usage body into the hub's normalized quota: a "5h" and a "7d" bucket.
func quotaResponse(body []byte) (pluginapi.QuotaFetchResponse, error) {
	type window struct {
		Utilization float64   `json:"utilization"`
		ResetsAt    time.Time `json:"resets_at"`
	}
	var usage struct {
		FiveHour *window `json:"five_hour"`
		SevenDay *window `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &usage); err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	var buckets []pluginapi.QuotaBucket
	for _, w := range []struct {
		name string
		u    *window
	}{{"5h", usage.FiveHour}, {"7d", usage.SevenDay}} {
		u := w.u
		if u == nil {
			continue
		}
		bucket := pluginapi.QuotaBucket{Window: w.name, RemainingFraction: 1 - u.Utilization/100}
		if !u.ResetsAt.IsZero() {
			bucket.ResetTime = u.ResetsAt.UTC().Format(time.RFC3339)
		}
		buckets = append(buckets, bucket)
	}
	return pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: "Claude", Buckets: buckets}}}, nil
}

// order ranks accounts soonest weekly reset first: what's left in a window when it resets is lost.
// Spent accounts go last, and accounts with no reading after them, so both stay a fallback.
func order(readings map[string]reading, ids []string, spent float64) []string {
	key := func(id string) float64 {
		r, ok := readings[id]
		switch {
		case !ok:
			return math.Inf(1)
		case r.used >= spent:
			return math.MaxFloat64
		default:
			return float64(r.reset.Unix())
		}
	}
	out := append([]string(nil), ids...)
	// Ties keep a fixed order, so one session doesn't alternate between equal accounts.
	sort.SliceStable(out, func(i, j int) bool {
		if ki, kj := key(out[i]), key(out[j]); ki != kj {
			return ki < kj
		}
		return out[i] < out[j]
	})
	return out
}
