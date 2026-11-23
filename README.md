# comin-hook

comin-hook is a small HTTP receiver.
It lets a CI job trigger an immediate fetch in [comin](https://github.com/nlewo/comin), the NixOS GitOps agent, on the host it runs on.

The CI job proves who it is with its OIDC token, so no shared secret exists anywhere.

The binary can also publish itself on the internet through the host's own tailscaled, with Tailscale Funnel.

## Request flow

1. The CI job gets a token with a custom audience from its OIDC provider.
2. It sends an empty `POST` request with that token as a bearer token.
3. The receiver checks the token, then calls `Fetch` on the gRPC socket of comin.
4. comin fetches the branch it already follows and deploys what it finds.

The receiver checks each request in this order, and stops at the first failure:

| Check | Failure |
|:--|:--|
| The path is exactly `--path` | 404 |
| The method is `POST` | 405 |
| An `Authorization: Bearer` header carries a token | 401 |
| The signature, issuer, audience and expiry are valid, and the token has a `jti` | 401 |
| The `sub` claim is one of the `--subject` values, and each `--claim` matches | 403 |
| The `jti` was not seen before its expiry | 409 |
| comin accepts the `Fetch` call | 502 |

A request that passes every check gets 202.
The log has one line per request with the outcome, the `sub` and the `jti`.
It never contains the token.

## Threat model

The request carries no data.
The hook only makes comin fetch the branch it already follows.
A forged or replayed request can therefore cause at most one extra fetch.

The hook still checks who sends each request.
It accepts only a token that the configured issuer signed, for the configured audience, with an allowed `sub` and the required claims.
It rejects a `jti` that it already saw, until the token expires.

The set of seen IDs lives in memory, so a restart forgets it.
A replay after a restart is possible until the token expires.
Such a replay still causes only an extra fetch.

The receiver reads no request body.
It limits headers and times out slow clients.

## Install

The repository is a Nix flake with the program as `packages.<system>.default`, for `x86_64-linux` and `aarch64-linux`.

```sh
nix build github:victor-software-house/comin-hook
```

## Commands

Each flag has an environment variable, which applies when the flag is not given.
The variable name is `COMIN_HOOK_` plus the flag name in upper case, with `-` as `_`.
A missing required value is an error at start.

### serve

`comin-hook serve` runs the receiver.

| Flag | Environment variable | Default | Meaning |
|:--|:--|:--|:--|
| `--listen` | `COMIN_HOOK_LISTEN` | `127.0.0.1:8371` | Address to listen on |
| `--path` | `COMIN_HOOK_PATH` | `/deploy` | The one request path |
| `--issuer` | `COMIN_HOOK_ISSUER` | `https://token.actions.githubusercontent.com` | OIDC issuer, an https URL |
| `--audience` | `COMIN_HOOK_AUDIENCE` | none, required | Audience that tokens must carry |
| `--subject` | `COMIN_HOOK_SUBJECT` | none, at least one | Accepted `sub` value, repeatable |
| `--claim` | `COMIN_HOOK_CLAIM` | none | Extra string claim as `key=value`, repeatable |
| `--comin-socket` | `COMIN_HOOK_COMIN_SOCKET` | `/run/comin-grpc.sock` | gRPC unix socket of comin |

In the environment, a repeatable flag takes comma-separated values.
A `--subject` is an exact match.
A `--claim` such as `event_name=push` or `ref=refs/heads/main` must equal the string claim of the token.

### publish and unpublish

`comin-hook publish` adds the receiver to the serve configuration of the local tailscaled and allows Funnel for it.
`comin-hook unpublish` removes what `publish` added.
Both are idempotent.

| Flag | Environment variable | Default | Meaning |
|:--|:--|:--|:--|
| `--listen` | `COMIN_HOOK_LISTEN` | `127.0.0.1:8371` | Address of the receiver, which Funnel proxies to |
| `--path` | `COMIN_HOOK_PATH` | `/deploy` | Path of the receiver, which Funnel serves |
| `--funnel-port` | `COMIN_HOOK_FUNNEL_PORT` | `443` | HTTPS port of Funnel: 443, 8443 or 10000 |
| `--tailscaled-socket` | `COMIN_HOOK_TAILSCALED_SOCKET` | `/var/run/tailscale/tailscaled.sock` | Unix socket of tailscaled |

`publish` reads the DNS name of the node from tailscaled.
Then it adds an HTTPS listener on `--funnel-port`, a handler at `--path` that proxies to the receiver, and Funnel for that host and port.

Every other entry of the serve configuration stays as it is, such as Tailscale Services and other ports.
The write carries the ETag of the configuration that `publish` read.
If another writer changed the configuration meanwhile, `publish` starts over.

Funnel applies to a whole host and port.
If that host and port already have a handler at another path, `publish` changes nothing and fails.
`unpublish` removes the handler it added.
It removes the Funnel entry and the listener only when no other handler remains on that host and port.

Funnel needs HTTPS enabled for the tailnet and the `funnel` node attribute for the host.
The public URL is `https://<node DNS name>:<funnel port><path>`, without the port part for 443.

## NixOS module

The flake provides `nixosModules.default`.
It sets nothing outside its own unit, `comin-hook.service`.
The unit runs `serve` as a dynamic user, with strict systemd hardening, after `comin.service`.
With `funnel.enable`, the unit also runs `publish` before it starts and `unpublish` after it stops, both as root, after `tailscaled.service`.

| Option | Type | Default | Meaning |
|:--|:--|:--|:--|
| `services.comin-hook.enable` | bool | `false` | Run the receiver |
| `services.comin-hook.package` | package | the flake package | The program to run |
| `services.comin-hook.listenAddress` | str | `"127.0.0.1"` | Address to listen on |
| `services.comin-hook.port` | port | `8371` | TCP port to listen on |
| `services.comin-hook.path` | str | `"/deploy"` | The one request path |
| `services.comin-hook.issuer` | str | `"https://token.actions.githubusercontent.com"` | OIDC issuer, an https URL |
| `services.comin-hook.audience` | str | none, required | Audience that tokens must carry |
| `services.comin-hook.subjects` | list of str | `[ ]`, at least one | Accepted `sub` values |
| `services.comin-hook.claims` | attrs of str | `{ }` | Extra claims with their required values |
| `services.comin-hook.cominSocket` | path | `"/run/comin-grpc.sock"` | gRPC unix socket of comin |
| `services.comin-hook.cominSocketGroup` | null or str | `null` | Group to join, when the group of the socket limits access |
| `services.comin-hook.funnel.enable` | bool | `false` | Publish the receiver with Tailscale Funnel |
| `services.comin-hook.funnel.port` | 443, 8443 or 10000 | `443` | HTTPS port of Funnel |

A minimal host configuration, with placeholder values:

```nix
{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
  inputs.comin-hook.url = "github:victor-software-house/comin-hook";
  inputs.comin-hook.inputs.nixpkgs.follows = "nixpkgs";

  outputs =
    { nixpkgs, comin-hook, ... }:
    {
      nixosConfigurations.example-host = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          comin-hook.nixosModules.default
          {
            # The host runs comin and tailscaled itself.
            services.tailscale.enable = true;

            services.comin-hook = {
              enable = true;
              audience = "https://example.com/comin-hook";
              subjects = [ "repo:example-org/example-repo:ref:refs/heads/main" ];
              claims.event_name = "push";
              funnel.enable = true;
            };
          }
        ];
      };
    };
}
```

## CI example

A GitHub Actions job needs `id-token: write`.
It fetches a token with a custom audience from `ACTIONS_ID_TOKEN_REQUEST_URL`, then posts it to the receiver.
The audience and the `sub` below match the host configuration above.

```yaml
name: trigger-comin
on:
  push:
    branches: [main]
permissions:
  contents: read
jobs:
  trigger:
    runs-on: ubuntu-latest
    permissions:
      id-token: write
    steps:
      - name: Make comin fetch
        env:
          AUDIENCE: https://example.com/comin-hook
          HOOK_URL: https://example-host.example.com/deploy
        run: |
          token=$(curl --silent --show-error --fail --get \
            --header "Authorization: Bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
            --data-urlencode "audience=${AUDIENCE}" \
            "${ACTIONS_ID_TOKEN_REQUEST_URL}" | jq --raw-output .value)
          curl --silent --show-error --fail --request POST \
            --header "Authorization: Bearer ${token}" \
            "${HOOK_URL}"
```

For a push to `main`, the token has the `sub` `repo:example-org/example-repo:ref:refs/heads/main` and the claim `event_name=push`.
GitHub documents the other `sub` formats, such as those for an environment or a pull request.

## Development

`mise run go:verify` runs the offline checks.
`nix build` builds the package and runs the tests in its check phase.

## License

comin-hook is licensed under the [GNU Affero General Public License v3.0](LICENSE) (`AGPL-3.0-only`).
