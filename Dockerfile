# AkerDock control plane — static binary in a distroless image (ADR-021).
#
# The image has no shell, no package manager and no libc: the attack surface is
# the binary and nothing else. That is also why the compose healthcheck calls
# `/akerdock healthcheck` instead of curl — there is no curl in here (§6.6).

# --platform=$BUILDPLATFORM everywhere below: the toolchains run natively and
# cross-compile/emit for the target. Building arm64 under QEMU emulation works
# but takes minutes instead of seconds, and neither Go nor the Angular CLI
# needs the target architecture to produce its output.

# The dashboard is built HERE, not taken from the checkout: internal/web/dist
# is a committed artefact of `make web`, and trusting it would let an image
# ship a dashboard older than the sources it sits next to. Building it in its
# own stage makes the image reflect web/src as it is — and keeps node and npm
# out of the Go stage entirely.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /web

# Dependencies first: the lockfile changes far less often than the sources, so
# npm ci is reused across most rebuilds.
COPY web/package.json web/package-lock.json ./
RUN npm ci --silent

COPY web/ .
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first: they change far less often than the code, so this layer
# is reused across most rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Replace the committed dashboard build with the one produced above — same
# copy `make web` performs, done from a stage instead of the working tree.
RUN rm -rf internal/web/dist
COPY --from=web /web/dist/akerdock-web/browser/ internal/web/dist/

# CGO off: the binary must not depend on the builder's libc, since the runtime
# image has none. The version is stamped in, not read from git — the build must
# be reproducible from a source tarball.
ARG VERSION=dev
# IMAGE is this build's own image reference (ADR-036): baked in so the
# scale-to-zero waker is deployed from the exact same image, with no runtime
# configuration. install.sh passes akerdock:${AKERDOCK_TAG}; empty falls back to
# AKERDOCK_IMAGE at runtime.
ARG IMAGE=
# COMMIT is the exact git commit this build was made from (ADR-078): a
# source-only install bakes it so every managed server can rebuild the SAME
# image from the public repository, natively for its own CPU. Deliberately
# empty on a dirty working tree — a build that matches no commit must not
# claim one.
ARG COMMIT=
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION} -X main.image=${IMAGE} -X main.commit=${COMMIT}" \
        -o /akerdock ./cmd/akerdock

FROM gcr.io/distroless/static-debian12:nonroot

# The binary reaches target servers over SSH and buckets over HTTPS: without
# the CA bundle every TLS handshake fails. distroless/static ships one, and
# ca-certificates.crt is where Go looks for it.
COPY --from=build /akerdock /akerdock

# nonroot (uid 65532) — the data volume is written by this user.
USER nonroot:nonroot
WORKDIR /var/lib/akerdock

EXPOSE 8080

# The four modes of ADR-021 via `serve` (ADR-033): all-in-one (default), api, worker, scheduler.
ENTRYPOINT ["/akerdock"]
CMD ["serve", "all-in-one"]
