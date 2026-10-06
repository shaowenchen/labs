# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
# CGO is off so the binary is static and carries no libc dependency of its own.
# The module is HTTP clients and an in-memory record of the sessions handed out
# — nothing in it needs cgo — so this is a choice about the artifact rather than
# a constraint of a dependency.
#
# The builder is pinned to $BUILDPLATFORM, and that is not a detail. Left
# unpinned, a multi-arch build pulls an image per target architecture and runs
# this whole stage — the module download and every compilation — under QEMU for
# each one that is not the runner's, which is slow enough to look like a hang.
# Pinned, the stage runs natively and Go cross-compiles, which it does at full
# speed: CGO_ENABLED=0 below means there is no C toolchain to emulate.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG BUILDPLATFORM
ARG TARGETPLATFORM
ARG TARGETOS=linux
ARG TARGETARCH

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

WORKDIR /src

# Dependencies first, so a source change does not refetch the module graph.
COPY go.mod ./
RUN go mod download

COPY . .

# A build that was not told when it happened records the time it happened.
#
# The default above cannot be a timestamp — an ARG default is evaluated before
# the build starts, so it would be the same value for every build — and a build
# invoked bare (`docker build .`, which is what `docker compose build` does) has
# nothing to fill it in. Without this the binary reports "unknown", the console
# drops the "built" line from its footer, and a deployment has no answer to
# when it was deployed — which is the one thing the line is for.
#
# It runs here rather than in the RUN below so the whole string is fixed before
# the ldflags are expanded. Only the time is guessed: the commit is a build arg
# because .git is excluded from the context (see .dockerignore) and cannot be
# recovered inside the image.
RUN set -eu; \
    if [ "${BUILD_TIME}" = "unknown" ] || [ -z "${BUILD_TIME}" ]; then \
      BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
    fi; \
    printf '%s' "${BUILD_TIME}" > /tmp/build_time

ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}

# The time is read back from the file the step above wrote rather than passed
# down as an ARG of this stage: `RUN` does not see a variable assigned by an
# earlier `RUN`, and re-declaring the ARG would pick up the unset default again.
RUN go build -trimpath \
      -ldflags "-s -w \
        -X github.com/shaowenchen/labs/internal/buildinfo.Version=${VERSION} \
        -X github.com/shaowenchen/labs/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/shaowenchen/labs/internal/buildinfo.BuildTime=$(cat /tmp/build_time)" \
      -o /out/labs ./cmd/labs

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
# Distroless rather than Alpine: the service speaks HTTP and keeps its state in
# memory. It runs no subprocess and needs no shell or writable filesystem, so
# there is nothing for a base with a package manager to provide, and a smaller
# base is a smaller thing to keep patched.
#
# static-debian12:nonroot defines uid 65532, so the container runs unprivileged
# without anything having to arrange it.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/labs /usr/local/bin/labs

# LABS_LOG_LEVEL is the only default worth baking in. LABS_LISTEN is deliberately
# NOT set here: the service falls back to PORT when it is unset, and an image
# that pinned LABS_LISTEN would override the PORT a platform injects — turning
# the variable the platform probes into a no-op. A bare `docker run` still binds
# :8080 through the fallback.
ENV LABS_LOG_LEVEL=info

USER nonroot:nonroot

EXPOSE 8080

# Distroless has no shell, so the binary is the entrypoint directly. It installs
# its own signal handling, which is what a shell wrapper would otherwise be for.
# It takes no subcommand — flags and environment only — so no CMD is needed.
ENTRYPOINT ["/usr/local/bin/labs"]
