# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
# CGO is off so the binary is static and carries no libc dependency of its own.
# The module is HTTP clients and a JSON file — nothing in it needs cgo — so this
# is a choice about the artifact rather than a constraint of a dependency.
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

WORKDIR /src

# Dependencies first, so a source change does not refetch the module graph.
COPY go.mod ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}

RUN go build -trimpath \
      -ldflags "-s -w \
        -X github.com/shaowenchen/labs/internal/buildinfo.Version=${VERSION} \
        -X github.com/shaowenchen/labs/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/labs ./cmd/labs

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
# Distroless rather than Alpine: the service talks HTTP and writes one JSON
# file. It runs no subprocess and needs no shell, so there is nothing for a base
# with a package manager to provide, and a smaller base is a smaller thing to
# keep patched.
#
# static-debian12:nonroot defines uid 65532, so the pod runs unprivileged
# without the deployment having to arrange it. Nothing needs writing to disk: the
# session state is in memory, deliberately, so the image runs read-only.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/labs /usr/local/bin/labs

# A bare `docker run` of the image starts somewhere sensible rather than failing
# on an empty configuration. The compose file and a real deployment set the rest.
ENV LABS_LISTEN=:8080 \
    LABS_LOG_LEVEL=info

USER nonroot:nonroot

EXPOSE 8080

# Distroless has no shell, so the binary is the entrypoint directly. It installs
# its own signal handling, which is what a shell wrapper would otherwise be for.
ENTRYPOINT ["/usr/local/bin/labs"]
