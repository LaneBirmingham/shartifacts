FROM node:26-alpine AS frontend
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY scripts/build.mjs scripts/ui.jsx scripts/tailwind-compat.js scripts/math.js scripts/
RUN npm run build

FROM golang:1.26-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal ./internal
COPY web ./web
COPY --from=frontend /src/web/vendor ./web/vendor
RUN CGO_ENABLED=0 go build -trimpath -o /shartifacts .

# The app runner sidecar: the same binary, with the Go toolchain that builds app backends.
# It runs them with only the apps directory mounted (compose.yaml), never the artifacts.
FROM golang:1.26-alpine AS runner
RUN addgroup -g 10001 app && adduser -D -u 10001 -G app app && mkdir /apps && chown app:app /apps
COPY --from=backend /shartifacts /usr/local/bin/shartifacts
ENV GOTOOLCHAIN=local CGO_ENABLED=0 SHARTIFACTS_RUNNER_LISTEN=:8090 SHARTIFACTS_APPS_DIR=/apps
USER app
EXPOSE 8090
VOLUME /apps
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q -O /dev/null http://127.0.0.1:8090/healthz || exit 1
ENTRYPOINT ["shartifacts", "-runner"]

# shartifacts itself (the default target). Configure it with SHARTIFACTS_* environment
# variables or flags after the image name; docs/configuration.md lists them.
FROM alpine:3.23
RUN addgroup -g 10001 app && adduser -D -u 10001 -G app app && mkdir /data && chown app:app /data
COPY --from=backend /shartifacts /usr/local/bin/shartifacts
ENV SHARTIFACTS_DATA=/data
USER app
EXPOSE 8080
VOLUME /data
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["shartifacts"]
