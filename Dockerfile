# The yuuka image: one static binary with the WebUI embedded, on distroless.
#
#   docker build -t yuuka:local .
#   docker build --build-arg VERSION=1.2.3 -t yuuka:1.2.3 .
#
# Base images are pinned by digest; the tag next to each digest is only for
# the reader. Bump both together. The build stages run on the build
# platform and cross-compile, so a multi-platform build never emulates Node
# or Go. Deployment templates are in deploy/.

# --- WebUI -------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM node:24.22.2-trixie-slim@sha256:54f84b4cf8432b21aaffd5d3ce8d41e12cea0d10d33c09d2f3c9879d49d6b446 AS web
WORKDIR /src
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0 npm_config_update_notifier=false
# pnpm-workspace.yaml carries the minimumReleaseAge and allowBuilds rules,
# so the install below obeys them exactly as `mise run js:install` does.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml .node-version ./
COPY web/package.json web/
COPY worker/package.json worker/
# .node-version is the single source of the Node version; fail on drift
# instead of building with a different one. Corepack installs the pnpm that
# package.json's packageManager names.
RUN test "$(node --version)" = "v$(cat .node-version)" \
 && corepack enable pnpm \
 && pnpm --version
# Only the WebUI's dependencies: the Worker's toolchain is not needed here.
RUN --mount=type=cache,id=pnpm-store,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile --filter yuuka-web
COPY web/ web/
RUN pnpm --dir web run build

# --- Go binary ---------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS build
WORKDIR /src
# mise.toml pins the Go version; fail on drift, and never fetch another
# toolchain behind our back.
ENV GOTOOLCHAIN=local CGO_ENABLED=0
COPY mise.toml ./
RUN grep -qx "go = \"$(go env GOVERSION | sed 's/^go//')\"" mise.toml
COPY go.mod go.sum ./
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/embed.go web/
COPY --from=web /src/web/dist/ web/dist/
ARG TARGETOS TARGETARCH
ARG VERSION=0.0.0-dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/yuuka ./cmd/yuuka \
 && mkdir /out/data

# --- Runtime -----------------------------------------------------------------
# static-debian13:nonroot: CA certificates and tzdata, no shell, uid 65532.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
LABEL org.opencontainers.image.source="https://github.com/yuuka-mail/yuuka" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/yuuka /yuuka
# Created here so a fresh named volume inherits the nonroot owner.
COPY --from=build --chown=65532:65532 /out/data /var/lib/yuuka
# Inside a container the loopback default (127.0.0.1:31480) is unreachable
# from the reverse proxy, so listen on every interface of the container's
# own network. Publishing the port to the host is the deployment's choice;
# deploy/compose.yaml does not.
ENV YUUKA_DATA_DIR=/var/lib/yuuka \
    YUUKA_LISTEN=0.0.0.0:31480
VOLUME /var/lib/yuuka
EXPOSE 31480
USER 65532:65532
ENTRYPOINT ["/yuuka"]
CMD ["serve"]
