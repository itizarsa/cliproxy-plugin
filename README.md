# cliproxy-reset-first

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) scheduler plugin: each Claude
request goes to the account whose weekly window resets soonest. Quota left in a window when it
resets is lost, so the account about to reset is spent first.

- Reset times come from the rate-limit headers the hub already records per account. An account
  with none yet is asked once through Anthropic's usage endpoint, via the hub's `api-call`.
- An account at 98% of its week drops to the back. One cooling down on its 5-hour limit isn't
  offered by the hub, so the next one takes over.
- The hub asks a scheduler plugin before its session affinity, and affinity doesn't bind the
  plugin's picks. The ranking is deterministic instead, so a session stays on one account (and its
  prompt cache) until that account is spent or its week resets.

## Install

```sh
make install
```

In the hub config (`$(brew --prefix)/etc/cliproxyapi.conf`):

```yaml
plugins:
  enabled: true
  dir: "~/.config/cliproxyapi/plugins"
  configs:
    reset-first:
      enabled: true
      priority: 1
      management_key: "..."     # management.secret-key
      # management_url: "http://127.0.0.1:8317"
      # refresh_seconds: 60
      # spent: 0.98
```

## After upgrading the hub

The plugin ABI is versioned, so rebuild against the hub's SDK:

```sh
brew upgrade cliproxyapi && make sdk install
```
