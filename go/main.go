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
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	defaultFlushIntervalSeconds = 300
	headerPrefix                = "anthropic-ratelimit-unified-"
)

var currentConfig atomic.Value // pluginConfig

var (
	stateMu         sync.Mutex
	state           = map[string]*accountState{}
	loadedStateFile string
)

var (
	cursorMu sync.Mutex
	cursors  = map[string]int{}
)

var flushOnce sync.Once

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type accountConfig struct {
	AuthID           string  `yaml:"auth_id"`
	Reserve5hPercent float64 `yaml:"reserve_5h_percent"`
	Reserve7dPercent float64 `yaml:"reserve_7d_percent"`
}

type pluginConfig struct {
	StateFile            string          `yaml:"state_file"`
	FlushIntervalSeconds int             `yaml:"flush_interval_seconds"`
	Accounts             []accountConfig `yaml:"accounts"`
}

// accountState is the latest parsed Anthropic quota utilization for one auth ID.
type accountState struct {
	Util5h    float64   `json:"util_5h"`
	Status5h  string    `json:"status_5h,omitempty"`
	Reset5h   int64     `json:"reset_5h,omitempty"`
	Util7d    float64   `json:"util_7d"`
	Status7d  string    `json:"status_7d,omitempty"`
	Reset7d   int64     `json:"reset_7d,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	Scheduler   bool `json:"scheduler"`
	UsagePlugin bool `json:"usage_plugin"`
}

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
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	flushState(loadedStateFile)
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginShutdown:
		flushState(loadedStateFile)
		return okEnvelope(struct{}{})
	case pluginabi.MethodSchedulerPick:
		return pickAuth(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}

	cfg := pluginConfig{}
	if len(req.ConfigYAML) > 0 {
		decoded, errDecode := decodeConfig(req.ConfigYAML)
		if errDecode != nil {
			return errDecode
		}
		cfg = decoded
	}
	cfg.StateFile = strings.TrimSpace(cfg.StateFile)
	if cfg.FlushIntervalSeconds <= 0 {
		cfg.FlushIntervalSeconds = defaultFlushIntervalSeconds
	}
	for i := range cfg.Accounts {
		cfg.Accounts[i].AuthID = strings.TrimSpace(cfg.Accounts[i].AuthID)
	}
	currentConfig.Store(cfg)

	if cfg.StateFile != "" {
		stateMu.Lock()
		needsLoad := cfg.StateFile != loadedStateFile
		stateMu.Unlock()
		if needsLoad {
			loadState(cfg.StateFile)
			stateMu.Lock()
			loadedStateFile = cfg.StateFile
			stateMu.Unlock()
		}
	}

	flushOnce.Do(func() {
		go flushLoop()
	})

	return nil
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	var cfg pluginConfig
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	return cfg, nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "quota-reserve",
			Version:          "0.1.0",
			Author:           "router-for-me",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			Logo:             "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/main/docs/logo.png",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "state_file",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Local JSON file path used to persist tracked quota utilization across restarts.",
				},
				{
					Name:        "flush_interval_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "How often (in seconds) tracked utilization is flushed to state_file.",
				},
				{
					Name:        "accounts",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "List of {auth_id, reserve_5h_percent, reserve_7d_percent} entries. Only listed auth IDs are subject to quota reservation; all other candidates are left untouched.",
				},
			},
		},
		Capabilities: registrationCapability{
			Scheduler:   true,
			UsagePlugin: true,
		},
	}
}

// handleUsage parses Anthropic's real quota-utilization headers (already
// forwarded unfiltered by the host on every UsageRecord) and updates the
// tracked snapshot for the record's AuthID.
func handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" || len(record.ResponseHeaders) == 0 {
		return okEnvelope(struct{}{})
	}

	updated := false
	stateMu.Lock()
	entry, ok := state[authID]
	if !ok {
		entry = &accountState{}
		state[authID] = entry
	}
	for key, values := range record.ResponseHeaders {
		if len(values) == 0 {
			continue
		}
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, headerPrefix) {
			continue
		}
		rest := strings.TrimPrefix(lower, headerPrefix)
		sep := strings.LastIndex(rest, "-")
		if sep <= 0 {
			continue
		}
		window := rest[:sep]
		field := rest[sep+1:]
		value := values[0]

		switch window {
		case "5h":
			switch field {
			case "utilization":
				if v, err := strconv.ParseFloat(value, 64); err == nil {
					entry.Util5h = v
					updated = true
				}
			case "status":
				entry.Status5h = value
				updated = true
			case "reset":
				if v, err := strconv.ParseInt(value, 10, 64); err == nil {
					entry.Reset5h = v
					updated = true
				}
			}
		case "7d":
			switch field {
			case "utilization":
				if v, err := strconv.ParseFloat(value, 64); err == nil {
					entry.Util7d = v
					updated = true
				}
			case "status":
				entry.Status7d = value
				updated = true
			case "reset":
				if v, err := strconv.ParseInt(value, 10, 64); err == nil {
					entry.Reset7d = v
					updated = true
				}
			}
		}
	}
	if updated {
		entry.UpdatedAt = time.Now()
	}
	stateMu.Unlock()

	return okEnvelope(struct{}{})
}

// pickAuth blocks configured auth IDs once they cross their reserved
// threshold and otherwise leaves scheduling untouched.
func pickAuth(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	cfg := loadedConfig()
	if len(cfg.Accounts) == 0 || len(req.Candidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	limits := make(map[string]accountConfig, len(cfg.Accounts))
	for _, acct := range cfg.Accounts {
		if acct.AuthID != "" {
			limits[acct.AuthID] = acct
		}
	}

	configuredPresent := false
	for _, candidate := range req.Candidates {
		if _, ok := limits[candidate.ID]; ok {
			configuredPresent = true
			break
		}
	}
	if !configuredPresent {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	stateMu.Lock()
	eligible := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		acct, configured := limits[candidate.ID]
		if !configured {
			eligible = append(eligible, candidate)
			continue
		}
		entry, tracked := state[candidate.ID]
		if !tracked {
			eligible = append(eligible, candidate)
			continue
		}
		blocked := false
		if acct.Reserve5hPercent > 0 && entry.Util5h >= 1-acct.Reserve5hPercent/100 {
			blocked = true
		}
		if acct.Reserve7dPercent > 0 && entry.Util7d >= 1-acct.Reserve7dPercent/100 {
			blocked = true
		}
		if !blocked {
			eligible = append(eligible, candidate)
		}
	}
	stateMu.Unlock()

	if len(eligible) == 0 {
		// All configured accounts are over budget and no fallback candidate
		// exists — hand back to the host's normal scheduler rather than
		// failing the request outright.
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	topPriority := eligible[0].Priority
	for _, c := range eligible[1:] {
		if c.Priority < topPriority {
			topPriority = c.Priority
		}
	}
	group := make([]pluginapi.SchedulerAuthCandidate, 0, len(eligible))
	for _, c := range eligible {
		if c.Priority == topPriority {
			group = append(group, c)
		}
	}

	cursorKey := req.Provider + "|" + req.Model
	cursorMu.Lock()
	idx := cursors[cursorKey] % len(group)
	cursors[cursorKey] = cursors[cursorKey] + 1
	cursorMu.Unlock()

	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  group[idx].ID,
		Handled: true,
	})
}

func loadedConfig() pluginConfig {
	raw := currentConfig.Load()
	if cfg, ok := raw.(pluginConfig); ok {
		return cfg
	}
	return pluginConfig{}
}

func flushLoop() {
	for {
		cfg := loadedConfig()
		interval := cfg.FlushIntervalSeconds
		if interval <= 0 {
			interval = defaultFlushIntervalSeconds
		}
		time.Sleep(time.Duration(interval) * time.Second)
		flushState(cfg.StateFile)
	}
}

func flushState(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	stateMu.Lock()
	snapshot := make(map[string]*accountState, len(state))
	for k, v := range state {
		cp := *v
		snapshot[k] = &cp
	}
	stateMu.Unlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func loadState(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var snapshot map[string]*accountState
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return
	}
	stateMu.Lock()
	for k, v := range snapshot {
		if v != nil {
			state[k] = v
		}
	}
	stateMu.Unlock()
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
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
