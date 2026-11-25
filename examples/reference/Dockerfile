# syntax=docker/dockerfile:1

# Build from the directory that holds go.mod. In this repository that is the
# root:
#
#   docker build -f examples/reference/Dockerfile -t reference .
#
# In a service copied from this example, go.mod sits beside the Dockerfile:
# build with `--build-arg PKG=./cmd/api .` or change the default below.
ARG PKG=./examples/reference/cmd/api

# Pinned by digest so a rebuild is the same build; Dependabot moves the pins.
FROM golang:1.27@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS build
ARG PKG
WORKDIR /src

# Dependencies first, so a source change does not download them again.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# CGO off: the SQLite driver is pure Go, so the binary is static and needs no
# libc in the final image.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api "$PKG"
# The final image has no shell to create this with.
RUN mkdir /out/data

# No shell, no package manager, CA certificates and time zones included.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/api /app/api
# Owned by the unprivileged user, so a volume mounted here is writable by it.
COPY --from=build --chown=nonroot:nonroot /out/data /data

ENV DATABASE_PATH=/data/reference.db
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot

# No HEALTHCHECK: there is nothing in the image to run one with. Point the
# orchestrator's probes at /healthz (alive) and /readyz (ready).
ENTRYPOINT ["/app/api"]
