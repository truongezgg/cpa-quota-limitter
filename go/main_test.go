package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPickAuthHardBlocksWhenAllCandidatesReserved(t *testing.T) {
	configurePickAuthTest(t, true)

	raw, errPick := pickAuth(schedulerRequestJSON(t, "limited"))
	if errPick != nil {
		t.Fatalf("pickAuth() error = %v", errPick)
	}
	var got envelope
	if errUnmarshal := json.Unmarshal(raw, &got); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if got.OK || got.Error == nil {
		t.Fatalf("pickAuth() response = %#v, want quota error", got)
	}
	if got.Error.Code != "anthropic_quota_reserve_exhausted" || got.Error.HTTPStatus != 429 {
		t.Fatalf("pickAuth() error = %#v", got.Error)
	}
}

func TestPickAuthSoftModeDelegatesWhenAllCandidatesReserved(t *testing.T) {
	configurePickAuthTest(t, false)

	raw, errPick := pickAuth(schedulerRequestJSON(t, "limited"))
	if errPick != nil {
		t.Fatalf("pickAuth() error = %v", errPick)
	}
	var got envelope
	if errUnmarshal := json.Unmarshal(raw, &got); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	var result pluginapi.SchedulerPickResponse
	if errUnmarshal := json.Unmarshal(got.Result, &result); errUnmarshal != nil {
		t.Fatalf("unmarshal result: %v", errUnmarshal)
	}
	if !got.OK || result.Handled {
		t.Fatalf("pickAuth() response = %#v, result = %#v", got, result)
	}
}

func TestPickAuthHardModeUsesUnconfiguredFallback(t *testing.T) {
	configurePickAuthTest(t, true)

	raw, errPick := pickAuth(schedulerRequestJSON(t, "limited", "fallback"))
	if errPick != nil {
		t.Fatalf("pickAuth() error = %v", errPick)
	}
	var got envelope
	if errUnmarshal := json.Unmarshal(raw, &got); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	var result pluginapi.SchedulerPickResponse
	if errUnmarshal := json.Unmarshal(got.Result, &result); errUnmarshal != nil {
		t.Fatalf("unmarshal result: %v", errUnmarshal)
	}
	if !got.OK || !result.Handled || result.AuthID != "fallback" {
		t.Fatalf("pickAuth() response = %#v, result = %#v", got, result)
	}
}

func configurePickAuthTest(t *testing.T, hardBlock bool) {
	t.Helper()
	currentConfig.Store(pluginConfig{
		HardBlockWhenAllReserved: hardBlock,
		Accounts: []accountConfig{{
			AuthID:           "limited",
			Reserve5hPercent: 20,
			Reserve7dPercent: 20,
		}},
	})
	stateMu.Lock()
	state = map[string]*accountState{
		"limited": {Util5h: 0.8, Reset5h: 4102444800},
	}
	stateMu.Unlock()
	t.Cleanup(func() {
		stateMu.Lock()
		state = map[string]*accountState{}
		stateMu.Unlock()
	})
}

func schedulerRequestJSON(t *testing.T, authIDs ...string) []byte {
	t.Helper()
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(authIDs))
	for _, authID := range authIDs {
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: authID, Provider: "claude"})
	}
	raw, errMarshal := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider:   "claude",
		Model:      "claude-sonnet",
		Candidates: candidates,
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	return raw
}
