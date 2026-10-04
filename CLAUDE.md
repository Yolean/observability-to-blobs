# Working in this repository

- Run the tests before every push: `./test.sh` (about 30 minutes, needs
  docker) for any change to code, manifests, `test.sh` or `itest/`;
  `./test.sh checks` at least for documentation only. Report what you ran
  and its result in the pull request. CI does not run automatically
  outside the upstream repository (README.md, "Running").
- A change to `compactor/` Go code, `go.mod`/`go.sum` or `compactor/sql/`
  moves the image digest: run `compactor/build.sh bump` and commit the
  pin; `compactor/build.sh check` must pass.
