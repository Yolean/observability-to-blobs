# developer-feed on a Mac

From a checkout of the `developer-feed` branch.

## Once per Cloudflare account

Dashboard: an Account API token with Workers R2 Storage Edit (short expiry, IP filter). This is the setup token.

## Per machine

```sh
brew install cmake pkg-config bison flex openssl@3 libyaml
developer-feed/macos/build.sh
developer-feed/cloudflare/setup-bucket.sh --account <account id> --bucket <bucket>   # setup token; creates the bucket on the first machine, finds it on the others
developer-feed/macos/install.sh
```

Check `CLUSTER_NAME` in `~/.config/observability-host/v1/env`: the same on all your machines (default: login name).

Dashboard, R2, Manage API tokens: a parent token for this machine, Object Read & Write, this bucket only, no expiry, no IP filter.

```sh
B=~/.local/opt/observability-host/bin
$B/dev-feed-r2 mint --parent-key-id <access key id> --ttl 1d | $B/dev-feed-r2 verify
$B/dev-feed-r2 mint --ttl 1d | $B/dev-feed-credentials store
developer-feed/macos/install.sh
tail -f ~/.local/state/observability-host/v1/log/agent.log
```

Renew: the two `mint` lines without `--parent-key-id`, then
`launchctl kickstart -k gui/$(id -u)/com.yolean.observability-host`.

Update: `build.sh`, then `install.sh`.

Lost machine: delete its parent token in the dashboard.

Stop: `developer-feed/macos/uninstall.sh` (`--purge` removes everything).
