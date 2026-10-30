# syntax=docker/dockerfile:1
# Cross-compiles on the build machine's own architecture (no QEMU needed),
# then copies one static binary into a distroless image. SQLite is compiled
# into the binary; there is no database server and nothing is stored in the
# image. Mount the zips at /data (read-only) and a host folder at /index.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ao3 ./cmd/ao3

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/ao3 /ao3
ENV AO3_DATA=/data \
    AO3_INDEX=/index/ao3.db \
    AO3_ADDR=:8080 \
    AO3_AUTO_INDEX=true \
    AO3_FTS_SUMMARY=false \
    AO3_INDEX_WORKERS=0
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 CMD ["/ao3", "healthcheck"]
LABEL org.opencontainers.image.source="https://github.com/mihai-max/ao3" \
      org.opencontainers.image.description="Search, read and download EPUBs from AO3 archive zips without extracting them" \
      org.opencontainers.image.licenses="MIT"
ENTRYPOINT ["/ao3"]
CMD ["serve"]
