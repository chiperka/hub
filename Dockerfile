# Chiperka Hub — control panel + UI, with chiperka-worker bundled for the
# optional embedded worker (CHIPERKA_LOCAL_WORKER=1).
#
#   docker build -t chiperka-hub .

ARG WORKER_IMAGE=finie/chiperka-worker:latest

FROM node:22-alpine AS ui
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.24-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/chiperka-hub .

FROM alpine:3.21 AS base
# docker-cli/curl/bash are used by the embedded chiperka-worker.
RUN apk add --no-cache ca-certificates tzdata docker-cli curl bash
COPY --from=build /out/chiperka-hub /usr/local/bin/chiperka-hub
ENV CHIPERKA_ADDR=:8080 \
    CHIPERKA_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/health >/dev/null || exit 1
ENTRYPOINT ["chiperka-hub"]

FROM ${WORKER_IMAGE} AS worker

FROM base AS hub
COPY --from=worker /usr/local/bin/chiperka-worker /usr/local/bin/chiperka-worker
