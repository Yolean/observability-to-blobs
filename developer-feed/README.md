# developer-feed

A developer's own feed: what their machines know about their work, shipped by the host agent
to a bucket of their own, which they use as their own datalake and may publish artifacts
from. Nobody is asked to run it; developers try it and keep it if it helps their work.

This tree lives on the long-lived branch `developer-feed`, rebased onto `main`:

- **No pull request from the branch,** and **no `v*` tag on it.** CI runs on pushes to `main`,
  on pull requests and on `v*` tags (`.github/workflows/`), so the branch runs no GitHub
  Actions. It is built and tested on the developer's machines.
- **What should go upstream** gets its own topic branch off `main` and a normal pull request
  with `./test.sh`. Candidates: the modular collectors, metrics shipped from the host build,
  and an upload hold for `out_s3`.
- **What stays here** is this directory. The branch adds files rather than editing shared ones,
  so rebases stay cheap.

Development and end-to-end tests run against versitygw in docker, the way `itest/host/` tests
the host agent, never against a developer's real bucket. A PutObject-only user there stands in
for the minted R2 credential.

All machines of one developer write to one bucket, in the host agent's key layout
(`HOST_MODE.md`):

```
/{KEY_ROOT}/{CLUSTER_NAME}/{YYYY-MM-DD}/{HH}/{NODE_NAME}/{MMSS}-{uuid}.jsonl.zst
```

- `KEY_ROOT` is `dev1`.
- `CLUSTER_NAME` names the feed (the developer), the same on all their machines.
- `NODE_NAME` names the machine.

| Directory | What |
|---|---|
| [`cloudflare/`](cloudflare/) | `dev-feed-r2`: write-only and read credentials for an R2 bucket, minted locally from a parent token per machine, and a check that they are what they claim |
