# Build from the repository root so the read-only HA view and its embedded UI
# always come from the same source tree and build identity.
FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG RELEASE_VERSION=dev
ARG RELEASE_COMMIT=unknown
ARG RELEASE_DATE=unknown
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X github.com/akz142857/Halro/internal/buildinfo.Version=${RELEASE_VERSION} -X github.com/akz142857/Halro/internal/buildinfo.Commit=${RELEASE_COMMIT} -X github.com/akz142857/Halro/internal/buildinfo.Date=${RELEASE_DATE}" \
    -o /out/halro-ha-health ./cmd/halro-ha-health

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/halro-ha-health /usr/local/bin/halro-ha-health
WORKDIR /var/lib/halro-ha-health
USER 65532:65532
EXPOSE 9105
ENTRYPOINT ["/usr/local/bin/halro-ha-health"]
