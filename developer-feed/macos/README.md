# developer-feed/macos: the user-level host agent on a Mac

The host agent (`HOST_MODE.md`), unprivileged, for one user, as a LaunchAgent, with the feed's
collectors (`../collectors/`). It ships this Mac's Claude Code, git and cmux activity to the
developer's own bucket. It needs no Apple Developer ID and no privacy permission: everything
it reads is the user's own files.

## Layout

The same split as the machine-level agent's `/opt`, `/etc` and `/var/lib`, in the user's
directories, with a version in the config and state paths (`../lib/config.sh`):

| What | Path | Machine-level |
|---|---|---|
| program | `~/.local/opt/observability-host/` | `/opt/observability-host/` |
| config | `~/.config/observability-host/v1/`: `env`, `args`, `aws-config` | `/etc/observability-host/` |
| credential | the login Keychain, service `observability-host-v1` | `/etc/observability-host/s3-credentials` |
| state | `~/.local/state/observability-host/v1/`: positions, buffers, the generated configuration, `log/agent.log` | `/var/lib/observability-host/` |

`OBSERVABILITY_HOST_PREFIX`, `OBSERVABILITY_HOST_CONFIG` and `OBSERVABILITY_HOST_STATE` move
them, which tests do. Nothing a run needs is kept in the checkout: `target/macos/` holds only
build output.

**The config** is maintained by the scripts:
- `env` holds `KEY=VALUE` lines that the agent sources. Every non-secret a script produces
  goes there, and the script says for each key whether it was added, changed (from what) or
  already so:

  | Script | What it writes to `env` |
  |---|---|
  | `setup-bucket.sh` | the account, bucket, jurisdiction, `S3_BUCKET`, `S3_ENDPOINT`, `S3_REGION` |
  | `install.sh` | `CLUSTER_NAME` (the login name), `NODE_NAME` (this Mac's LocalHostName, fixed once), and every other setting with its value, where not already set |
  | `dev-feed-r2 mint` | the parent token's Access Key ID |
  | `dev-feed-r2 verify` | when the write credential passed |
  | `dev-feed-credentials store` | the credential's expiry |

  Edits stay: a script changes a key only when it has a new value for it.
- `args` holds the collectors' flags. The host's `journal`, `files`, `node` and `self` are off,
  so the agent listens on no port.
- `aws-config` holds a single `credential_process` line.

**No secret is ever written to a file.**

`~/Library/LaunchAgents/com.yolean.observability-host.plist` runs
`~/.local/opt/observability-host/bin/dev-feed-agent` at login and keeps it running. The plist
raises the open-files limit, because `tail` keeps every transcript open and launchd's default
is 256. It is a plain plist in the user's LaunchAgents, unsigned. macOS shows a "Background
Items Added" notice the first time.

## Setup

From a checkout of the `developer-feed` branch, none of these needs arguments once the bucket
is known:

```sh
brew install cmake pkg-config bison flex openssl@3 libyaml   # once
developer-feed/macos/build.sh                                # target/macos/: fluent-bit, dev-feed-r2, dev-feed-git
developer-feed/cloudflare/setup-bucket.sh --account <id> --bucket <name>   # once; asks for the setup token
developer-feed/macos/install.sh                              # program and config; says what is missing
```

Then a parent token for this Mac, in the dashboard: R2 → Manage API tokens → Create Account API
token, with Object Read & Write, this bucket only, no expiry and no IP filter. Mint, verify and
store the credential in your own terminal. `mint` asks for the parent's secret with echo off,
and keeps its Access Key ID in the config, so the next mint needs no flag:

```sh
B=~/.local/opt/observability-host/bin
$B/dev-feed-r2 mint --parent-key-id <id> --ttl 1d | $B/dev-feed-r2 verify
$B/dev-feed-r2 mint --ttl 1d | $B/dev-feed-credentials store
developer-feed/macos/install.sh                              # again: now it starts the agent
tail -f ~/.local/state/observability-host/v1/log/agent.log
```

**Updating:** `build.sh`, then `install.sh`. These keep the config and the state.

**Stopping:** `uninstall.sh` stops the agent and keeps everything else. `uninstall.sh --purge`
also removes the program, the config, the state and the Keychain item.

## The build

`build.sh` builds the fork's fluent-bit natively: the Dockerfile's `FLB_VERSION`, the fork's
patches, then this build's own `patches/`. What macOS needs differs from the Linux build in
these ways:

| Change | Why |
|---|---|
| `patches/0001-aws-credential-process-on-darwin.patch` | fluent-bit builds `credential_process` on Linux only, "might work for other Unix systems". It does, with an `#include <signal.h>` added |
| `-DFLB_SECURITY=Off`, with `-fstack-protector-strong -D_FORTIFY_SOURCE=2` instead | with `FLB_RELEASE` it adds GNU ld's `-z relro`, `-z now` and `-z noexecstack` on any platform, which Apple's ld refuses |
| `-DHAVE_PIPE2=0` | the SDK marks `pipe2` available from macOS 27; c-ares falls back to `pipe()` |
| no `systemd` input, no jemalloc, no Go plugins | none of them on this platform |

The binary links Homebrew's OpenSSL and libyaml from `/opt/homebrew/opt/`, which stays valid
across `brew upgrade`. `target/macos/BUILD` records the commit it was built from, and whether
the tree had uncommitted changes. `install.sh` copies that record to the program directory.

## Credential

The minted credential is the `credential_process` JSON of `dev-feed-r2 mint`:

- **Kept:** `dev-feed-credentials store` keeps it, base64-encoded, as the password of a generic
  password item in the login Keychain: service `observability-host-v1`, account the user. It
  hands the item to `security -i` on stdin, so the credential is never in a command line or a
  file, then reads it back to check it.
- **Used:** fluent-bit runs `dev-feed-credentials` (`aws-config`) when it first needs
  credentials, and again near the credential's `Expiration`.
- **Expiry:** after it, uploads fail and the rows wait in the state's buffers until a new
  credential is stored and the agent restarted
  (`launchctl kickstart -k gui/$(id -u)/com.yolean.observability-host`).

**This is an interim.** An item that `security` made can be read by any process of the user
that runs `security`, a Claude Code session included. It is still better than a file, which
`cat` and backups pick up. The design is for a helper to own the item instead, so that any
other reader causes a dialog.

## Tested

Run on this Mac (arm64, macOS 26), against a local versitygw in docker as a PutObject-only
user, in temporary directories (`OBSERVABILITY_HOST_*`, `OBSERVABILITY_HOST_LAUNCHD=off`):
- the native build;
- `install.sh`;
- `dev-feed-agent` in the foreground, its credential from a test `credential_process`.

It shipped this user's real transcript lines, prompt history, memories, cmux journal rows and
the remotes of every repository in the history. From a scratch repository it shipped a commit,
with its `commit` and `update by push` reflog entries, and the heartbeat with the Mac's boot
id. It listened on no port and stopped within a second of SIGTERM.

`dev-feed-git` reads reflogs as files: a `git` per ref took 2 min 53 s here, where one
repository has 7,376 refs; it now takes under 2 s.

## Not verified

- A run under launchd with a credential from the Keychain, against R2.
- `security -i` keeping no history of the commands it reads.

## Known

- **Children inherit fluent-bit's sockets.** Commands that fluent-bit's `exec` input runs
  inherit its listening sockets, which are not close-on-exec. A child still running when
  fluent-bit is killed keeps the port, and the next start cannot bind it. With no port
  open (`node` and `self` off) and commands that finish in seconds, nothing is affected.
