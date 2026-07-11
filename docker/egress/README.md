# kahya-egress-fwd sidecar image (W3-05)

`mcp/shell.EgressNetworkEnsurer` starts a `kahya-egress-fwd` sidecar
container attached to both the `kahya-egress` (`--internal`, no route out)
Docker network and the default `bridge` network. It is a **dumb TCP
forward** — a single `socat TCP-LISTEN:3128,fork,reuseaddr
TCP:host.docker.internal:<egress.port>` invocation — the ONLY route out of
`kahya-egress`. All policy (allowlist, budget, sensitive-read block) lives
in `kahyad`'s Go code (`kahyad/internal/egress`), never in this sidecar.

## Image pin

Unlike `docker/sandbox` (a **locally built** image, pinned by image ID —
a purely local build has no registry digest), this sidecar is the public
**`alpine/socat`** image from Docker Hub, pinned by its real **registry
digest** in `IMAGE_DIGEST` — a proper supply-chain pin: `docker run`
always resolves `alpine/socat@sha256:<pinned>` explicitly, never a mutable
`:latest` tag.

To re-pin after a deliberate upgrade:

```sh
docker pull alpine/socat
docker image inspect alpine/socat --format '{{index .RepoDigests 0}}'
# copy the sha256:... portion into IMAGE_DIGEST
```

`EgressNetworkEnsurer.Ensure` refuses to start the sidecar at all
(fail-closed, mirrors `mcp/shell.Runner.PinnedDigest`'s identical
convention for the sandbox image) until `IMAGE_DIGEST` holds a non-empty
value.

## Reachability

`Ensure` also asserts `host.docker.internal` resolves from **inside** the
running sidecar (`docker exec kahya-egress-fwd getent hosts
host.docker.internal`) — colima's virtiofs/network setup (or a
misconfigured Docker Desktop network) can otherwise leave a sidecar that
starts cleanly but can never actually reach `kahyad`'s egress proxy. A
failure here surfaces as the Turkish `Egress ağı kurulamadı` error, never
a silent container-egress timeout.
