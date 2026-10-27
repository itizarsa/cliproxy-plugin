// Command reset-first is a CLIProxyAPI scheduler plugin that sends each request to the Claude
// account whose weekly window resets soonest, so no window resets with quota left unused. It is
// also the hub's quota provider for Claude, so `POST /v0/management/quota/fetch` answers.
//
// The hub asks a scheduler plugin before its own session affinity, and affinity only binds the
// accounts it picks itself. Picking deterministically from one ranking keeps a session on one
// account (and its prompt cache) anyway: it moves only when that account is spent or its week
// resets, as affinity would.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type config struct {
	ManagementURL string  `yaml:"management_url"`
	ManagementKey string  `yaml:"management_key"`
	RefreshSecs   int     `yaml:"refresh_seconds"`
	Spent         float64 `yaml:"spent"`
}

var (
	currentConfig atomic.Pointer[config]
	ranking       atomic.Pointer[[]string]
	startRefresh  sync.Once
	client        = &http.Client{Timeout: 30 * time.Second}
	// Probed readings, kept until their reset passes so an idle account is asked about once.
	// Quota fetches arrive on the hub's goroutines, so the maps are guarded.
	probeMu  sync.Mutex
	probes   = map[string]reading{}
	probedAt = map[string]time.Time{}
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var body []byte
	if request != nil && requestLen > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handle(C.GoString(method), body)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handle(method string, body []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(body); err != nil {
			return nil, err
		}
		startRefresh.Do(func() { go refreshLoop() })
		return okEnvelope(registration())
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		return okEnvelope(pick(req))
	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": "claude"})
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(pluginapi.QuotaDescribeResponse{SupportedProviders: []string{"claude"}, DisplayName: "Claude usage"})
	case pluginabi.MethodQuotaFetch:
		var req pluginapi.QuotaFetchRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		resp, err := fetchQuota(currentConfig.Load(), req)
		if err != nil {
			return nil, err
		}
		return okEnvelope(resp)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(body []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
	}
	cfg := config{ManagementURL: "http://127.0.0.1:8317", RefreshSecs: 60, Spent: 0.98}
	if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
		return err
	}
	cfg.ManagementURL = strings.TrimRight(cfg.ManagementURL, "/")
	currentConfig.Store(&cfg)
	return nil
}

func registration() any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name:             "reset-first",
			Version:          "0.1.0",
			Author:           "arshath",
			GitHubRepository: "https://github.com/itizarsa/cliproxy-plugin",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "management_url", Type: pluginapi.ConfigFieldTypeString, Description: "The hub's own address, for reading account quota."},
				{Name: "management_key", Type: pluginapi.ConfigFieldTypeString, Description: "Management API secret."},
				{Name: "refresh_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "How often the ranking is rebuilt."},
				{Name: "spent", Type: pluginapi.ConfigFieldTypeNumber, Description: "Weekly utilization (0-1) at which an account counts as spent."},
			},
		},
		"capabilities": map[string]bool{"scheduler": true, "quota_provider": true},
	}
}

// pick sends Claude requests to the best-ranked account the hub offers. The hub only offers
// accounts it can use now, so one cooling down on its 5-hour limit is skipped here too.
// A request with no Claude candidate is left to the built-in scheduler.
func pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	rank := ranking.Load()
	if rank == nil {
		return pluginapi.SchedulerPickResponse{}
	}
	// Claude Code's route reaches the scheduler as "mixed", so filter by each candidate's provider.
	offered := make(map[string]bool, len(req.Candidates))
	for _, c := range req.Candidates {
		if c.Provider == "claude" {
			offered[c.ID] = true
		}
	}
	for _, id := range *rank {
		if offered[id] {
			return pluginapi.SchedulerPickResponse{AuthID: id, Handled: true}
		}
	}
	return pluginapi.SchedulerPickResponse{}
}

func refreshLoop() {
	var last string
	for {
		cfg := currentConfig.Load()
		wait := time.Duration(cfg.RefreshSecs) * time.Second
		rank, err := refresh(cfg)
		if err != nil {
			// The hub isn't serving yet at startup; try again soon.
			logf("refresh: %v", err)
			wait = time.Second
		} else {
			ranking.Store(&rank)
			if joined := strings.Join(rank, ", "); joined != last {
				logf("order: %s", joined)
				last = joined
			}
		}
		time.Sleep(wait)
	}
}

func refresh(cfg *config) ([]string, error) {
	var list struct {
		Files []authFile `json:"files"`
	}
	if err := manage(cfg, "GET", "auth-files", nil, &list); err != nil {
		return nil, err
	}
	now := time.Now()
	readings := map[string]reading{}
	var ids []string
	for _, f := range list.Files {
		if f.Provider != "claude" || f.Disabled {
			continue
		}
		ids = append(ids, f.ID)
		if r, ok := observed(f, now); ok {
			readings[f.ID] = r
		} else if r, ok := probe(cfg, f, now); ok {
			readings[f.ID] = r
		}
	}
	return order(readings, ids, cfg.Spent), nil
}

// probe asks Anthropic's usage endpoint through the hub, which fills in the account's token,
// for an account the hub has no current headers for. At most every 30 minutes per account.
func probe(cfg *config, f authFile, now time.Time) (reading, bool) {
	probeMu.Lock()
	if r, ok := probes[f.ID]; ok && r.reset.After(now) {
		probeMu.Unlock()
		return r, true
	}
	if now.Sub(probedAt[f.ID]) < 30*time.Minute {
		probeMu.Unlock()
		return reading{}, false
	}
	probedAt[f.ID] = now
	probeMu.Unlock()
	body, err := usage(cfg, f.AuthIndex)
	if err != nil {
		logf("usage probe for %s failed: %v", f.Email, err)
		return reading{}, false
	}
	return remember(f.ID, body)
}

// fetchQuota answers the hub's quota fetch with a fresh reading, which the ranking also takes.
func fetchQuota(cfg *config, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	body, err := usage(cfg, req.AuthIndex)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	remember(req.AuthID, body)
	return quotaResponse(body)
}

func remember(id string, body []byte) (reading, bool) {
	r, ok := parseUsage(body)
	if ok {
		probeMu.Lock()
		probes[id] = r
		probeMu.Unlock()
	}
	return r, ok
}

// usage asks Anthropic's usage endpoint as the account, through the hub's api-call.
func usage(cfg *config, authIndex string) ([]byte, error) {
	var resp struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	err := manage(cfg, "POST", "api-call", map[string]any{
		"auth_index": authIndex,
		"method":     "GET",
		"url":        "https://api.anthropic.com/api/oauth/usage",
		"header":     map[string]string{"Authorization": "Bearer $TOKEN$", "anthropic-beta": "oauth-2025-04-20"},
	}, &resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return []byte(resp.Body), nil
}

func manage(cfg *config, method, path string, body, out any) error {
	var payload *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	} else {
		payload = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, cfg.ManagementURL+"/v0/management/"+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.ManagementKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[reset-first] "+format+"\n", args...)
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
