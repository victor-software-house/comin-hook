# comin-hook

comin-hook is a small HTTP receiver.
It lets a CI job trigger an immediate fetch in [comin](https://github.com/nlewo/comin), the NixOS GitOps agent, on the host it runs on.

The CI job proves who it is with its OIDC token, so no shared secret exists anywhere.

The program links only the Go standard library and this module.
The NixOS module can also publish the receiver on the internet with Tailscale Funnel, through the tailscale CLI of the host.

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
| The token passes the checks in [Token checks](#token-checks) | 401 |
| The `sub` claim is one of the `--subject` values, and each `--claim` matches | 403 |
| The `jti` was not seen before its expiry | 409 |
| comin accepts the `Fetch` call | 502 |

A request that passes every check gets 202.
The log has one line per request with its status and reason.
It adds the `sub` and the `jti` when the token got far enough to show them.
It never contains the token.

## Token checks

The receiver verifies the token itself, with the standard library.

1. The token has three base64url parts without padding, and is at most 8 KiB.
2. The header `alg` is exactly `RS256`, and it has a `kid`.
   `none`, `HS256` and every other algorithm are rejected.
3. At start, the receiver reads `<issuer>/.well-known/openid-configuration`.
   The `issuer` field must equal `--issuer` exactly, and `jwks_uri` must be an https URL.
4. It loads the signature keys of type Rivest-Shamir-Adleman (RSA) from `jwks_uri`, and ignores the others.
   Each request has a 10 second timeout and a 1 MiB limit on the body.
5. A `kid` that the receiver does not know makes it load the keys again.
   So does a verification after the loaded keys are an hour old.
6. The signature is an RSA signature over the first two parts, with SHA-256.
7. The claims must satisfy all of these:
   - `iss` equals `--issuer`.
   - `aud` is a string, or a list, that contains `--audience`.
   - `exp` is present and in the future.
   - `nbf` and `iat`, when present, are at most 60 seconds in the future.
   - `jti` and `sub` are non-empty strings.

The receiver loads the keys at most once a minute across all requests, and a bad signature never loads them.
A key that the issuer dropped stops verifying within an hour.

When the load of an old key set fails, the receiver keeps the old keys for 24 hours more.
An outage of the issuer shorter than that does not reject valid tokens.

After that, the key set is stale, and the receiver rejects every token with the reason `key set is stale`.
Each request still tries the load again once a minute, and the first load that works restores service.

A failed load costs speed and not a deploy, because comin polls its repository on its own.
A key set with no usable key counts as a failed load.

## Threat model

The request carries no data.
The hook only makes comin fetch the branch it already follows.
A forged or replayed request can therefore cause at most one extra fetch.

The hook still checks who sends each request.
It accepts only a token that the configured issuer signed, for the configured audience, with an allowed `sub` and the required claims.
It rejects a `jti` that it already saw, until the token expires.

The receiver reserves the `jti` before it calls comin.
It releases the `jti` when the call fails, so the job may retry with the same token, and keeps it after a success.

The set of reserved IDs lives in memory, so a restart forgets it.
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

`serve` is the only subcommand.
Each flag has an environment variable, which applies when the flag is not given.
The variable name is `COMIN_HOOK_` plus the flag name in upper case, with `-` as `_`.
A missing required value is an error at start.

### serve

`comin-hook serve` runs the receiver.
It calls comin's `Fetch` method, `/protobuf.Comin/Fetch`, as one unary gRPC call over HTTP/2 on the unix socket, with a 5 second deadline.

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

## NixOS module

The flake provides `nixosModules.default`.
It adds `comin-hook.service`, which runs `serve` as a dynamic user, with strict systemd hardening, after `comin.service`.
The module reads the gRPC socket and its group from the `services.comin.grpc` options, so it needs comin's NixOS module on the host.

With `funnel.enable`, it adds `comin-hook-funnel.service`, which needs `services.tailscale.enable`.
That unit is a oneshot that stays active, and it stops with `comin-hook.service`.
It starts after `tailscaled.service`, and starts again every 10 seconds after a failure.

Before it publishes anything, the unit runs two checks:

1. It waits up to 40 seconds until `tailscale status --json` shows `BackendState` `Running` and a DNS name for the node.
   Before the first network map, the node lacks the funnel attribute, and the CLI exits 0 without changing anything.
2. It reads `tailscale serve status --json` and fails with a message when the port serves anything besides the receiver.
   Funnel applies to a whole host and port, so another handler on the port would become public too.
   The check fails for a handler at another path, for a TCP forward, for plain HTTP and for TLS-terminated TCP on the port.

Then it runs:

```sh
tailscale funnel --bg --yes --https=<port> --set-path=<path> http://<listen><path>
```

The proxy target repeats the path, because tailscaled strips the mount point from the request path.

When the command returns, the unit checks `tailscale serve status --json` again.
It fails unless Funnel is on for the port and a proxy handler at the path points at the receiver.
The check is needed because the tailscale CLI exits 0 without changing anything when the node lacks the funnel attribute.

Stopping the unit runs `tailscale serve --https=<port> --set-path=<path> off`, and ignores a failure of that command.
That command removes the handler, and Funnel with it when no handler remains on the port.

Unlike `tailscale funnel ... off`, it does not check the funnel attribute first, so it also works on a node that lost the attribute.
A stop with tailscaled down, or with no handler left, leaves no failed unit.

The jq programs of these checks are files in `nix/`, and `nix flake check` runs them against the JSON fixtures in `nix/fixtures/`.

Funnel needs HTTPS enabled for the tailnet and the `funnel` node attribute for the host.
The public URL is `https://<node DNS name>:<funnel port><path>`, without the port part for 443.

| Option | Type | Default | Meaning |
|:--|:--|:--|:--|
| `services.comin-hook.enable` | bool | `false` | Run the receiver |
| `services.comin-hook.package` | package | the flake package | The program to run |
| `services.comin-hook.listenAddress` | str | `"127.0.0.1"` | Address to listen on |
| `services.comin-hook.port` | port | `8371` | TCP port to listen on |
| `services.comin-hook.path` | str | `"/deploy"` | The one request path: segments of letters, digits and `. _ ~ -`, never `.` or `..`, with no trailing slash |
| `services.comin-hook.issuer` | str | `"https://token.actions.githubusercontent.com"` | OIDC issuer, an https URL |
| `services.comin-hook.audience` | str | none, required | Audience that tokens must carry |
| `services.comin-hook.subjects` | list of str | `[ ]`, at least one | Accepted `sub` values |
| `services.comin-hook.claims` | attrs of str | `{ }` | Extra claims with their required values |
| `services.comin-hook.cominSocket` | path | `services.comin.grpc.unix_socket_path` | gRPC unix socket of comin |
| `services.comin-hook.cominSocketGroup` | null or str | `services.comin.grpc.unix_socket_group`, or `null` when that is empty | Group to join, when the group of the socket limits access |
| `services.comin-hook.funnel.enable` | bool | `false` | Publish the receiver with Tailscale Funnel, which needs `services.tailscale.enable` |
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
            # The host runs tailscaled, and comin with its own NixOS module.
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
`nix flake check` evaluates a host that runs the module with Funnel, comin and tailscaled.

## License

comin-hook is licensed under the [GNU Affero General Public License v3.0](LICENSE) (`AGPL-3.0-only`).
