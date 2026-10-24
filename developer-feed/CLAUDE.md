# Working in developer-feed/

- This tree is on the branch `developer-feed`, which runs no CI (README.md). Test what you
  change here on the machine, and put what you ran and its result in the commit message:
  there is no pull request to report it in.
  - `cloudflare/`: `gofmt -l .`, `go vet ./...` and `go test ./...` in that directory.
- Develop and test end to end against versitygw in docker, as `itest/host/` does: a
  PutObject-only user under a bucket policy stands in for the minted R2 credential. The
  developer's real bucket is never used for development; `dev-feed-r2 verify` against it is
  a setup step, run by the developer.
- Never open a pull request from `developer-feed`, and never tag it `v*`.
- A change meant for `main` goes on its own topic branch off `main`, under the root
  `CLAUDE.md`'s rules (`./test.sh`).
- `cloudflare/sigv4.go` and `sigv4_test.go` are copies of `compactor/`'s; edit the
  compactor's and run `go generate` in `cloudflare/`.
- Never write a parent token's secret or a minted credential to a file, a test fixture or a
  commit. The tests use made-up values.
