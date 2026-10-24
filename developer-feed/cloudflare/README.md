# developer-feed/cloudflare: R2 credentials for a feed

`dev-feed-r2` mints and checks the credentials that agents use to write to a developer feed on
Cloudflare R2, and that the developer uses to read it. It is one Go module with no
dependencies beyond the standard library and `stty`, so it builds and runs the same on macOS
and Linux. Storing a minted credential is each platform's job, not this tool's (see "Where
the credential goes").

It starts where the manual part ends: R2 enabled on the account, and the feed's bucket
created.

## Why temporary credentials

R2's long-lived API tokens can't be write-only. Their four levels, Admin or Object, each Read
& Write or Read only, all include read and list, and can be scoped to buckets but not to
prefixes (developers.cloudflare.com/r2/api/tokens).

A **temporary credential** signed locally can be narrower
(developers.cloudflare.com/r2/api/s3/temporary-credentials). It derives from a parent token,
and lists S3 actions and key prefixes. `dev-feed-r2 mint` signs one with
`actions: ["PutObject"]` and `prefixPaths: ["{root}/{cluster}/"]`: it can add objects under
the feed's prefix and do nothing else.

- **The derivation follows R2's own example**
  (developers.cloudflare.com/r2/examples/authenticate-r2-temp-credentials):
  1. An HS256 JWT keyed with the parent's secret, with claims `bucket`, `actions`, `paths`,
     `sub` (the account), `iss` (the parent's key id), `aud` (the endpoint host), `iat`,
     `exp`.
  2. The temporary secret is the SHA-256 hex of the JWT.
  3. The session token is `base64("jwt/" + JWT)`.
  4. The parent's key id is reused.
- **The parent's secret** is used for the signature and is in nothing the tool prints or
  writes.

`actions` is only available with local signing, which is why the tool signs locally and does
not call the Temporary Credentials API. That API offers only the four preset scopes, all of
which include read.

## One bucket, many machines

Every machine writes to the same bucket with the host agent's key layout (`HOST_MODE.md`):

```
/{KEY_ROOT}/{CLUSTER_NAME}/{YYYY-MM-DD}/{HH}/{NODE_NAME}/{MMSS}-{uuid}.jsonl.zst
```

| Segment | For a developer feed | Default |
|---|---|---|
| `KEY_ROOT` | `dev1`, the feed's root, never shared with a production root | `dev1` |
| `CLUSTER_NAME` | the feed: one per developer, for example their name. All of that developer's machines use the same value | none: required, as in the agents |
| `NODE_NAME` | the machine | the host name, in the agent |

**A credential can be scoped to a cluster, not to a machine.** The machine's segment comes
after the date, so no prefix narrows a credential to one machine. Every machine's credential
can write anywhere under `dev1/{cluster}/`. That is harmless for a write-only credential that
cannot list or read, and moving the machine above the date would break every fixed-depth
reader of the layout.

**Revocation is per machine instead, through the parent.** Give each machine **its own parent
token**. Deleting or rolling a parent kills every credential minted from it at once, so a
lost laptop costs one click in the dashboard and leaves the other machines running.

## Setup

### 1. A parent token per machine

In the Cloudflare dashboard, under R2, manage the R2 API tokens and create one:

- **Permission:** Object Read & Write. A temporary credential can't exceed its parent, and
  the read role (below) needs read.
- **Buckets:** only the feed's bucket.
- **Name:** after the machine, for example `dev-feed-parent-<machine>`.
- **TTL:** forever. The minted credentials expire; the parent is revoked by deleting it.

The dashboard shows an **Access Key ID** and a **Secret Access Key**. The key id isn't secret:
it is also the minted credential's key id. Keep the secret in a password manager, or nowhere.
A lost parent secret is replaced by rolling the token and minting again. Never store it on
the machine.

The dashboard's exact labels may differ from these; the permission and bucket scope are what
matter.

### 2. Build

```sh
cd developer-feed/cloudflare
go build -o dev-feed-r2 .
```

### 3. The feed's settings

```sh
export DEV_FEED_ACCOUNT=<account id>    # the dashboard's account id, also in the R2 S3 endpoint
export DEV_FEED_BUCKET=<bucket>
export DEV_FEED_JURISDICTION=eu         # only if the bucket was created in the EU jurisdiction
export CLUSTER_NAME=<feed>              # the same on every machine of this developer
```

`KEY_ROOT` (default `dev1`) and `--endpoint` override the rest. `--prefix` replaces
`{root}/{cluster}/` altogether.

### 4. Mint and verify, with nothing on disk

```sh
./dev-feed-r2 mint --parent-key-id <parent access key id> --ttl 1h | ./dev-feed-r2 verify
```

`mint` asks for the parent's secret on the terminal with echo off. If its stdin is not a
terminal, it reads the secret from stdin instead, so a password manager's CLI can supply it:

```sh
op read 'op://<vault>/<item>/secret' | ./dev-feed-r2 mint --parent-key-id <id> | ./dev-feed-r2 verify
```

`mint` refuses to print a credential to a terminal (`--to-terminal` overrides that), so a
credential only ever goes into a pipe. `verify` reads the credential from stdin and checks it
against the bucket:

| Check | Must be |
|---|---|
| PutObject under the prefix | allowed |
| GetObject, HeadObject, DeleteObject of that object | denied |
| ListObjectsV2 of the bucket, and of the prefix | denied |
| PutObject outside the prefix | denied |
| CopyObject and CreateMultipartUpload under the prefix | denied |
| PutObject without the session token | denied |

This is the host agent's put-only check (`HOST_MODE.md`, "What the itest verifies"). Any
other answer exits non-zero.

The PutObject leaves one small object, `{prefix}.verify/{stamp}-{random}.txt`, which the
credential cannot delete. Its depth differs from the raw layout's, so readers' fixed-depth
globs (`dev1/{cluster}/*/*/*/*.jsonl.zst`) never match it. Delete them with the dashboard when
convenient.

### 5. Find the longest lifetime R2 accepts

R2 documents no maximum for a locally signed credential: its example defaults to one hour, and
`exp` is the signer's choice. `mint`'s default is `--ttl 30d`. Try longer ones with `verify`,
for example `--ttl 90d` and `--ttl 365d`, and keep the longest that passes. A credential R2
won't accept fails the first check.

## Where the credential goes

`mint`'s output is the JSON an AWS `credential_process` prints:

```json
{"Version":1,"AccessKeyId":"…","SecretAccessKey":"…","SessionToken":"…","Expiration":"…"}
```

fluent-bit's AWS profile provider reads that JSON (`src/aws/flb_aws_credentials_process.c`),
and its signer sends the session token as `x-amz-security-token`. The agent therefore gets
its credential from a command that prints this JSON. Where the JSON is kept between mints
belongs to each platform:

- **macOS:** a Keychain item owned by the developer-feed helper, whose `credentials`
  subcommand is the `credential_process`. Not built yet.
- **Linux:** a systemd credential, for example `systemd-creds encrypt`, loaded by the unit as
  `HOST_MODE.md`'s unit loads its key today. Not built yet.

Until then, a minted credential lives only in the pipe.

**Renewal** means minting again before `Expiration`, with the machine's parent. Renewal can't
be automatic without keeping the parent's secret on the machine, which would defeat its
purpose.

## Reading the feed

```sh
./dev-feed-r2 mint --role read --parent-key-id <id> --ttl 8h | ./dev-feed-r2 verify --role read
```

This mints `GetObject`, `HeadObject` and `ListObjectsV2` under the same prefix, for `y-obs`,
DuckDB or any S3 client that takes a session token. `verify --role read` checks:

- a listing of the prefix is allowed;
- PutObject, DeleteObject, and a listing outside the prefix are denied.

## Tests

```sh
go vet ./... && go test ./...
```

The tests check the derivation against R2's example, and run `verify` against a fake R2. The
fake authorizes as the docs describe temporary credentials:

- the session token's JWT is signed by the parent;
- the request is signed with the SHA-256 of that JWT;
- the call is one of the JWT's actions, under one of its prefixes.

This shows that minting, signing and checking agree with each other. That R2 agrees is shown
only by running step 4 against the real bucket.

`sigv4.go` and `sigv4_test.go` are copies of `compactor/`'s, kept identical by
`sigv4_sync_test.go`. Run `go generate` here after the compactor's changes.

## Not verified

- The longest `--ttl` R2 accepts (step 5).
- Whether R2 holds a read credential's `ListObjectsV2` to its prefixes the way the fake does.
- The dashboard's exact labels for creating the parent token.
