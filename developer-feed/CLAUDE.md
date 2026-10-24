# Working in developer-feed/

- This tree is on the branch `developer-feed`, which runs no CI (README.md). Test what you
  change here on the machine, and put what you ran and its result in the commit message:
  there is no pull request to report it in.
  - `cloudflare/`, `git/` and `backfill/`: `gofmt -l .`, `go vet ./...` and `go test ./...`
    in each.
  - `bin/`: `shellcheck bin/*` and `shellcheck itest/run.sh`.
  - `explore/`: `shellcheck -x -P SCRIPTDIR explore/explore.sh`; run it against a local
    versitygw with `OBSERVABILITY_HOST_*` temporary and `DEV_FEED_EXPLORE_CREDENTIAL_COMMAND`
    printing a test credential, never against the real bucket.
  - `macos/` and `lib/`: `shellcheck -x -P SCRIPTDIR` on every script. `build.sh` (no
    arguments) builds into `target/macos/`. Run `install.sh` and `dev-feed-agent` only with the
    `OBSERVABILITY_HOST_PREFIX`, `_CONFIG` and `_STATE` directories temporary and
    `OBSERVABILITY_HOST_LAUNCHD=off`, so that nothing touches the real config, state,
    `~/Library/LaunchAgents` or a running agent.
- Never write state into the checkout: a run's state goes in the user-level layout
  (`lib/config.sh`), and `target/` holds build output only. Every non-secret a script
  produces goes into the config with `cfg_set`, which says what it changed; no secret goes
  into any file.
- Develop and test end to end against versitygw in docker, as `itest/host/` does: a
  PutObject-only user under a bucket policy stands in for the minted R2 credential. The
  developer's real bucket is never used for development; `dev-feed-r2 verify` against it is
  a setup step, run by the developer.
- Never open a pull request from `developer-feed`, and never tag it `v*`.
- A change meant for `main` goes on its own topic branch off `main`, under the root
  `CLAUDE.md`'s rules (`./test.sh`).
- `cloudflare/` and `backfill/` have copies of `compactor/`'s `sigv4.go` and `sigv4_test.go`;
  edit the compactor's and run `go generate` in each.
- `dev-feed-backfill --ship` writes to a bucket for good (the credential cannot delete): run it
  against versitygw, never the developer's real bucket. The developer runs it there.
- Never write a parent token's secret or a minted credential to a file, a test fixture or a
  commit. The tests use made-up values.
