# developer-feed on a Mac

From a checkout of the `developer-feed` branch.

## Per Cloudflare account

Create a setup token in the Cloudflare dashboard, for `setup-bucket.sh`. It expires; create a new one when a machine needs it:

1. Manage Account, Account API Tokens: Create Token, Custom token.
2. Token name: `dev-feed-setup`.
3. Policy: Entire Account, Workers R2 Storage: Edit.
4. Token expiration: 7 days.
5. Client IP address filtering: Allow, this machine's public IP.
6. Review token, Create token, then copy the token. `setup-bucket.sh` asks for it.

## Per machine

```sh
brew install cmake pkg-config bison flex openssl@3 libyaml
developer-feed/macos/build.sh
developer-feed/cloudflare/setup-bucket.sh --account <account id> --bucket <bucket>   # setup token; creates the bucket on the first machine, finds it on the others
developer-feed/macos/install.sh
```

Create this machine's parent token in the Cloudflare dashboard. Its secret is only needed for the `mint` lines below, then forget it; the token itself must stay, as deleting it stops the machine's credential:

1. R2 object storage, Overview, Account Details: Manage next to API Tokens.
2. Create Account API token.
3. Token name: the machine, e.g. `dev-feed-smack3`.
4. Permissions: Object Read & Write.
5. Specify bucket(s): Apply to specific buckets only, and select the bucket.
6. TTL: Forever.
7. Client IP Address Filtering: none.
8. Create Account API token, then copy the Access Key ID and the Secret Access Key for the `mint` lines below.

```sh
B=~/.local/opt/observability-host/bin
$B/dev-feed-r2 mint --parent-key-id <access key id> --ttl 1d | $B/dev-feed-r2 verify
$B/dev-feed-r2 mint --ttl 1d | $B/dev-feed-credentials store
developer-feed/macos/install.sh
tail -f ~/.local/state/observability-host/v1/log/agent.log
```

Renew: a new parent token (steps above), the two `mint` lines with its Access Key ID,
`launchctl kickstart -k gui/$(id -u)/com.yolean.observability-host`, then delete the old parent token.

Update: `build.sh`, then `install.sh`.

Lost machine: delete its parent token in the dashboard.

Stop: `developer-feed/macos/uninstall.sh` (`--purge` removes everything).
