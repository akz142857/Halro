# syntax=docker/dockerfile:1.7

FROM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/ ./
RUN npm run build

FROM golang:1.26.5-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist/ ./internal/webui/dist/
ARG RELEASE_VERSION=dev
ARG RELEASE_COMMIT=unknown
ARG RELEASE_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/akz142857/Heimdall/internal/buildinfo.Version=${RELEASE_VERSION} -X github.com/akz142857/Heimdall/internal/buildinfo.Commit=${RELEASE_COMMIT} -X github.com/akz142857/Heimdall/internal/buildinfo.Date=${RELEASE_DATE}" \
    -o /out/heimdall ./cmd/heimdall
RUN mkdir -p /rootfs/var/lib/heimdall

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/heimdall /usr/local/bin/heimdall
COPY --from=build --chown=65532:65532 /rootfs/var/lib/heimdall/ /var/lib/heimdall/
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /usr/share/licenses/heimdall/
WORKDIR /var/lib/heimdall
USER 65532:65532
VOLUME ["/var/lib/heimdall"]
EXPOSE 8080 8081 9090
ENV HEIMDALL_HEALTH_URL=http://127.0.0.1:8080/health/ready
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/heimdall", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/heimdall"]
CMD ["serve", "--config", "/etc/heimdall/config.yaml"]
