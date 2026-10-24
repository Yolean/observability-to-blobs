# developer-feed/macos: the feed's agent on a Mac

The host agent (`HOST_MODE.md`), running per user as a LaunchAgent, with the feed's collectors
(`../collectors/`). It ships this Mac's Claude Code, git and cmux activity to the developer's
own bucket. It needs no Apple Developer ID and no privacy permission: everything it reads is
the user's own files.

## What goes where

Everything is under `~/.dev-feed` (`DEV_FEED_PREFIX`), copied by `install.sh`, never linked
to the checkout:

| Path | What |
|---|---|
| `opt/bin/fluent-bit` | the fork's fluent-bit, built for macOS by `build-fluent-bit.sh` |
| `opt/bin/observability-host-launch`, `observability-host-heartbeat` | the host agent's launcher and heartbeat |
| `opt/etc/` | `host.yaml`, the host's collectors and parts |
| `developer-feed/collectors/` | the feed's collectors (`HOST_COLLECTOR_DIRS`) |
| `developer-feed/bin/` | `dev-feed-git`, built at install; `dev-feed-cmux`; `dev-feed-agent`, what launchd runs; `dev-feed-credentials` |
| `env` | the settings: the feed, the bucket and its endpoint. No secrets |
| `args` | the collectors' flags: the host's `journal`, `files` and `node` off |
| `aws-config` | one line: `credential_process` is `dev-feed-credentials` |
| `state/` | positions, buffers, the generated configuration |
| `log/agent.log` | fluent-bit's log, at `warn` (`HOST_LOG_LEVEL`) |

`~/Library/LaunchAgents/com.yolean.dev-feed.plist` runs `dev-feed-agent` at login and keeps it
running (`KeepAlive`). It is a plain plist in the user's LaunchAgents, unsigned. macOS shows
a "Background Items Added" notice the first time, and lists the item under
`dev-feed-agent` in System Settings, General, Login Items & Extensions.

## Setup

**1. Build fluent-bit.** This needs Homebrew:

```sh
brew install cmake pkg-config bison flex openssl@3 libyaml
developer-feed/macos/build-fluent-bit.sh          # target/macos/fluent-bit
```

**2. Install.**

```sh
developer-feed/macos/install.sh
```

The first install writes `~/.dev-feed/env` with empty settings and doesn't start anything.

**3. Settings.** In `~/.dev-feed/env`:
- `CLUSTER_NAME`: the feed, the same on all of this developer's machines;
- `S3_BUCKET`;
- `S3_ENDPOINT`: for R2, `https://<account id>[.eu].r2.cloudflarestorage.com`.

`NODE_NAME` defaults to the Mac's LocalHostName.

**4. Credential.** Mint a write-only credential with this machine's parent token
(`../cloudflare/README.md`), check it, then store it. Each in a pipe, never in a file:

```sh
cd developer-feed/cloudflare && go build -o dev-feed-r2 .
./dev-feed-r2 mint --parent-key-id <id> --ttl 30d | ./dev-feed-r2 verify
./dev-feed-r2 mint --parent-key-id <id> --ttl 30d | ~/.dev-feed/developer-feed/bin/dev-feed-credentials store
```

**5. Start.**

```sh
developer-feed/macos/install.sh                  # again: now it loads the LaunchAgent
launchctl print gui/$(id -u)/com.yolean.dev-feed
tail -f ~/.dev-feed/log/agent.log
```

**Updating:** run `install.sh` again, which keeps `env`, `args` and `state/`.

**Stopping:** `uninstall.sh` stops the agent and keeps the files. `uninstall.sh --purge` also
removes `~/.dev-feed` and the Keychain item.

## Credential

The minted credential is the `credential_process` JSON of `dev-feed-r2 mint`.
`dev-feed-credentials store` keeps it, base64-encoded, as the password of a generic password
item in the login Keychain: service `dev-feed-put`, account the user. It hands the item to
`security -i` on stdin, so the credential is never in a command line or a file.

fluent-bit runs `dev-feed-credentials` (`aws-config`) when it first needs credentials, and
again near the credential's `Expiration`. When the credential has expired, uploads fail and
the rows wait in the buffers (`state/`) until a new one is stored and the agent restarted:
`launchctl kickstart -k gui/$(id -u)/com.yolean.dev-feed`. `dev-feed-credentials expiry`
says when the current one expires.

**This is an interim.** An item that `security` made can be read by any process of the user
that runs `security`, a Claude Code session included. It is still better than a file, which
`cat` and backups pick up. The feed's Swift helper is to own the item instead, so that any
other reader causes a dialog (the design notes, section 9).

## Not verified

- The build and a run on this Mac with real R2. So far `install.sh` and `dev-feed-agent` are
  checked only with a stub in place of fluent-bit.
- `security -i` keeping no history of the commands it reads.
