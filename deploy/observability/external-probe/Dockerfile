FROM golang:1.26.5-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/heimdall-deadman ./cmd/heimdall-deadman
COPY internal/deadman ./internal/deadman
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/heimdall-deadman ./cmd/heimdall-deadman
RUN mkdir -p /rootfs/var/lib/heimdall-deadman /rootfs/var/log/heimdall-deadman

FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35
COPY --from=build /out/heimdall-deadman /usr/local/bin/heimdall-deadman
COPY --from=build --chown=65532:65532 /rootfs/var/lib/heimdall-deadman/ /var/lib/heimdall-deadman/
COPY --from=build --chown=65532:65532 /rootfs/var/log/heimdall-deadman/ /var/log/heimdall-deadman/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/heimdall-deadman"]
