# hop and session-manager-plugin on Alpine: non-root, no package manager, no setuid
# binaries. Base images are pinned by digest; Dependabot proposes the updates.

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS hop
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/morganseznec/hop/internal/cli.version=${VERSION}" \
      -o /out/hop ./cmd/hop

# The plugin is built from its tagged sources rather than taken from AWS's packages:
# a static binary, the current Go toolchain, and a commit checked below.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS plugin
ARG TARGETOS TARGETARCH
ARG PLUGIN_TAG=1.2.896.0
ARG PLUGIN_COMMIT=6430675a44032a551be1937fb27a2d3fdfeb27f7
RUN apk add --no-cache git
RUN git clone --depth 1 --branch "${PLUGIN_TAG}" https://github.com/aws/session-manager-plugin.git /plugin \
 && test "$(git -C /plugin rev-parse HEAD)" = "${PLUGIN_COMMIT}"
WORKDIR /plugin
# Upstream builds in GOPATH mode with vendored dependencies (see its makefile).
ENV GO111MODULE=off GOPATH=/gopath:/plugin/vendor
RUN mkdir -p /gopath/src/github.com/aws/session-manager-plugin \
 && cp -r src /gopath/src/github.com/aws/session-manager-plugin/ \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" \
      -o /out/session-manager-plugin ./src/sessionmanagerplugin-main/main.go

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
LABEL org.opencontainers.image.title="hop" \
      org.opencontainers.image.description="Reach any EC2 instance in any of your AWS accounts and regions through Session Manager" \
      org.opencontainers.image.source="https://github.com/morganseznec/hop" \
      org.opencontainers.image.licenses="MIT"
# apk is removed once the base is up to date. Its database stays, so image scanners
# still see every installed package.
RUN apk upgrade --no-cache \
 && addgroup -S -g 65532 hop \
 && adduser -S -D -H -u 65532 -G hop -h /home/hop -s /sbin/nologin hop \
 && mkdir -p /home/hop/.aws \
 && chown -R hop:hop /home/hop \
 && find / -xdev -type f -perm /6000 -exec chmod a-s {} + \
 && rm -rf /sbin/apk /etc/apk /var/cache/apk /usr/share/apk /usr/lib/libapk* /lib/libapk*
COPY --from=plugin /out/session-manager-plugin /usr/local/bin/session-manager-plugin
COPY --from=hop /out/hop /usr/local/bin/hop
USER 65532:65532
# Cache and state go to /tmp, so the image also runs as the host user
# (--user "$(id -u):$(id -g)"), which is what makes a mounted ~/.aws readable on Linux.
ENV HOME=/home/hop \
    XDG_CACHE_HOME=/tmp/hop/cache \
    XDG_STATE_HOME=/tmp/hop/state
WORKDIR /tmp
ENTRYPOINT ["/usr/local/bin/hop"]
