# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist/ ./internal/webui/dist/
ARG RELEASE_VERSION=dev
ARG RELEASE_COMMIT=unknown
ARG RELEASE_DATE=unknown
ARG MODEL_CATALOG_TRUST_ROOTS=
ARG SOURCE_DATE_EPOCH=0
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X github.com/akz142857/Halro/internal/buildinfo.Version=${RELEASE_VERSION} -X github.com/akz142857/Halro/internal/buildinfo.Commit=${RELEASE_COMMIT} -X github.com/akz142857/Halro/internal/buildinfo.Date=${RELEASE_DATE} -X github.com/akz142857/Halro/internal/modelcatalog.ReleaseTrustRoots=${MODEL_CATALOG_TRUST_ROOTS}" \
    -o /out/halro ./cmd/halro
RUN mkdir -p /rootfs/var/lib/halro

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/halro /usr/local/bin/halro
COPY --from=build --chown=65532:65532 /rootfs/var/lib/halro/ /var/lib/halro/
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /usr/share/licenses/halro/
WORKDIR /var/lib/halro
USER 65532:65532
VOLUME ["/var/lib/halro"]
EXPOSE 8080 8081 9090
ENV HALRO_HEALTH_URL=http://127.0.0.1:8080/health/ready
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/halro", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/halro"]
CMD ["serve", "--config", "/etc/halro/config.yaml"]
