# docker/sandbox/Dockerfile — kahya-sandbox: the W3-04 Docker shell
# sandbox image every model-written shell script runs inside by default
# (HANDOFF §5 safety #6). The base image is pinned by DIGEST (not just a
# tag), so an upstream re-push of the "bookworm-slim" tag can never
# silently change what kahyad executes; docker/sandbox/IMAGE_DIGEST
# (committed) additionally pins THIS image's own built digest —
# mcp/shell/runner.go's DigestChecker refuses to start a container if the
# local kahya-sandbox image ever drifts from that committed value.
#
# Deliberately minimal: coreutils, git, python3, jq, curl — nothing else.
# curl (W3-05) is the standard tool the egress-proxy acceptance tests
# (mcp/shell/egress_integration_test.go) and manual verification use to
# prove "in-container curl allowlist'i atlayamıyor" — a needs_network:true
# job's ONLY route out is HTTP_PROXY/HTTPS_PROXY pointed at the
# kahya-egress-fwd sidecar; curl itself is not a security boundary
# (HANDOFF §5 safety #6: "ikili-allowlist güvenlik sınırı değil") — the
# boundary is --network none (default) / --network kahya-egress
# (needs_network:true, an --internal Docker network with no route out at
# all) / --read-only / the single rw workdir bind-mount, not which
# binaries happen to be on PATH. Growing this list means editing this
# file (reviewed in a commit), never a runtime config knob.
FROM debian:bookworm-slim@sha256:60eac759739651111db372c07be67863818726f754804b8707c90979bda511df

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        coreutils \
        git \
        python3 \
        jq \
        curl \
    && rm -rf /var/lib/apt/lists/*

# Non-root, fixed uid/gid 1000 — matches the --user 1000:1000 flag
# runner.go always passes (defense in depth; the flag is the real
# enforcement point, not this image-level default).
RUN groupadd --gid 1000 kahya \
    && useradd --uid 1000 --gid 1000 --create-home --shell /usr/sbin/nologin kahya

USER kahya
WORKDIR /work
