# comin-hook

A Go program that receives a CI job's OIDC-authenticated request and makes comin fetch.
See [README.md](./README.md) for what it does and how to run it.

## Rules

1. **The repository is public.**
   Keep every deployment-specific value out of tracked files, commit messages, branch names and pull-request text: tailnet names, host names, IP addresses, organization or repository IDs, names of private repositories, and descriptions of private infrastructure.
   A deployment value is a flag, an environment variable or a NixOS option.
   Examples use `example.com` and `example-org/example-repo`.
2. **Go layout.**
   One package per concept, named for its job, with no `util` package.
   `cmd/comin-hook` is the one composition root: `main` calls `run(ctx) error` and is the only caller of `os.Exit`.
   Wrap errors with `%w`.
3. **No new dependency without a reason.**
   Approve a module by adding it to the depguard `allow` list in the pull request that adds it to `go.mod`.
4. **Tests are table tests compared with go-cmp.**
5. **CI, tasks and hooks come from the templates repository.**
   Change them through a template update, never by hand-writing a workflow.
6. **Commits and pull requests.**
   Conventional Commits, one pull request per change, branch names `<type>/<short-topic>`.
   CI verifies each push; run `mise run go:verify` where the machine guide allows builds.
