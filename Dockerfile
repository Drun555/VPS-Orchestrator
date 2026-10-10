# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/vps-orchestrator .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && addgroup -g 10001 orchestrator && adduser -D -H -u 10001 -G orchestrator orchestrator && mkdir /data && chown 10001:10001 /data
COPY --from=build /out/vps-orchestrator /usr/local/bin/vps-orchestrator
ENV DATA_DIR=/data LISTEN_ADDRESS=0.0.0.0 PORT=42345
USER 10001:10001
EXPOSE 42345
VOLUME /data
ENTRYPOINT ["/usr/local/bin/vps-orchestrator"]
