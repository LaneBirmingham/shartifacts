# The image Gitea Actions jobs run in. It carries what this repository's checks need — the
# Go and Node versions the Dockerfile builds with, plus the docker CLI with buildx and the
# compose plugin, which reach the host daemon through the mounted socket — so a job never
# depends on what happens to be installed on the machine. buildx is not optional: the
# Dockerfile builds with BuildKit cache mounts, and `docker compose build` falls back to the
# classic builder, which cannot run them, when the plugin is missing.
#
# Build it on the server, where the runner lives:
#   docker build -f deploy/ci/Dockerfile -t shartifacts-ci:local .
#
# Rebuild it after a dependency change, not only after a toolchain one: the module cache is
# part of the image, so `go` never downloads anything. Rebuild after adding a dependency and
# the next job has it; leave it and the job fetches the difference on its own.
FROM golang:1.26-alpine

RUN apk add --no-cache \
      bash \
      docker-cli \
      docker-cli-buildx \
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

# The locked modules, so a job starts with every dependency compiled into reach. A job's
# container is discarded afterwards; its Go build cache is not (the runner mounts one), and
# this keeps the module cache from being the one thing that still needs the network.
COPY go.mod go.sum /tmp/deps/
RUN cd /tmp/deps && go mod download && rm -rf /tmp/deps
