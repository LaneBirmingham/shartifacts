# The image Gitea Actions jobs run in. It carries what this repository's checks need — the
# Go and Node versions the Dockerfile builds with, plus the docker CLI and compose plugin,
# which reach the host daemon through the mounted socket — so a job never depends on what
# happens to be installed on the machine.
#
# Build it on the server, where the runner lives:
#   docker build -f deploy/ci/Dockerfile -t shartifacts-ci:local .
FROM golang:1.26-alpine

RUN apk add --no-cache \
      bash \
      docker-cli \
      docker-cli-compose \
      gcc \
      git \
      musl-dev \
      nodejs \
      npm \
      openssh-client \
      rsync

# The race detector needs cgo, so this is 1 rather than the 0 the release build uses.
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=1
