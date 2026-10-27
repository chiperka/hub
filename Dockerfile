# Chiperka Hub — control panel + UI. Workers run as separate containers
# (finie/chiperka-worker).
#
#   docker build -t chiperka-hub .

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

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/chiperka-hub /usr/local/bin/chiperka-hub
ENV CHIPERKA_ADDR=:8080 \
    CHIPERKA_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/health >/dev/null || exit 1
ENTRYPOINT ["chiperka-hub"]
