# comin-hook

comin-hook is a small HTTP receiver that lets a CI job trigger an immediate fetch in [comin](https://github.com/nlewo/comin), the NixOS GitOps agent, on the host it runs on.
The CI job proves who it is with its OIDC token, so no shared secret exists anywhere.
The binary can also publish itself on the internet through the host's own tailscaled, with Tailscale Funnel.
