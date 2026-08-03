# Anthropic Quota Reserve Plugin

Reserves a percentage of Anthropic's real 5h/7d subscription quota for specific
auth IDs (e.g. an account borrowed from someone else) so the proxy stops
selecting that account once it gets close to being drained.

It implements:

- `plugin.register`
- `plugin.reconfigure`
- `plugin.shutdown`
- `scheduler.pick`
- `usage.handle`

## How it works

Every Claude response carries Anthropic's own `anthropic-ratelimit-unified-5h-utilization`
and `anthropic-ratelimit-unified-7d-utilization` headers (0.0–1.0 fraction of quota used,
alongside matching `-status` and
`-reset` headers). CLIProxyAPI already forwards these headers unfiltered to
every `UsagePlugin` via `UsageRecord.ResponseHeaders`; this plugin reads them
in `usage.handle`, keyed by `UsageRecord.AuthID`, and keeps the latest snapshot
per account in memory.

In `scheduler.pick`, only auth IDs listed in `accounts` are subject to this
logic — every other candidate is left completely untouched, so your own
unlimited accounts are unaffected. A configured account is excluded from
selection once its tracked 5h or 7d utilization crosses `1 - reserve_percent/100`.
If every candidate ends up excluded (e.g. all configured accounts are over
budget and there's no unconfigured fallback), the default soft mode returns
`handled: false` so the host's normal scheduler still picks something. Set
`hard_block_when_all_reserved: true` to fail the request with HTTP 429 instead,
ensuring a protected account is never selected below its reserve. Among
remaining eligible candidates, the plugin mimics host priority grouping and
round-robins within the top priority group.

This is a **soft budget** based on Anthropic's own self-reported utilization,
not a hard rate limiter — it only reacts after utilization has been observed
at least once (a newly added, never-used account is always eligible until its
first response is recorded).

## Known limitation: unbounded block if `reset` is never captured

Once an account is blocked it receives zero further traffic, so its tracked
state can only self-heal by comparing `now` against the `reset` timestamp
parsed from Anthropic's `-reset` header (see `pickAuth` in `go/main.go`). If
that header is ever missing or fails to parse for a window at the moment the
account trips over threshold, `Reset5h`/`Reset7d` stays `0`, and the account
is blocked **permanently** — including across restarts, since state is
persisted to `state_file`. There is currently no time-based fallback.

Mitigation today: if an account seems stuck, check `state_file` for the
account's entry and its `reset_5h`/`reset_7d` fields; a `0` value with a
high `util_5h`/`util_7d` confirms this case. Manual recovery is to edit or
remove that entry from `state_file` (or delete the file) and restart.

## Configuration

Add the plugin under `plugins.configs`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    anthropic-quota-reserve:
      enabled: true
      priority: 1
      state_file: "./plugins/anthropic-quota-reserve-state.json"
      flush_interval_seconds: 300
      hard_block_when_all_reserved: true
      accounts:
        - auth_id: "<Auth.ID of the borrowed account>"
          reserve_5h_percent: 20
          reserve_7d_percent: 20
```

Fields:

- `state_file`: local JSON file the plugin periodically flushes tracked
  utilization to, and reloads on startup, so state survives restarts.
- `flush_interval_seconds`: flush period; defaults to `300` (5 minutes) if unset or `<= 0`.
- `hard_block_when_all_reserved`: when `true`, return HTTP 429 instead of
  delegating to the built-in scheduler if every candidate is below reserve.
  Defaults to `false` for backward compatibility.
- `accounts`: list of `auth_id` / `reserve_5h_percent` / `reserve_7d_percent`
  entries. Find `auth_id` via the management API's auth listing endpoint — it
  is `Auth.ID`, which is stable across restarts. Accounts not listed here are
  never touched by this plugin.

## Build

From this directory:

```bash
cd go
go build -buildmode=c-shared -o ../anthropic-quota-reserve.so .
```

Copy `anthropic-quota-reserve.so` into your CLIProxyAPI `plugins/` dir, set
`plugins.enabled: true`, and add the `anthropic-quota-reserve` config block
above to your `config.yaml`.
