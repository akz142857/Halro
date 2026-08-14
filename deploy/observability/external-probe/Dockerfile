FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS build
ARG SOURCE_DATE_EPOCH=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Only what the dead-man itself imports. The narrow list is the point: this
# image exists to keep watching when the gateway cannot, so it must not
# silently acquire the gateway's dependencies. A test asserts this list still
# matches the binary's actual import graph.
COPY cmd/halro-deadman ./cmd/halro-deadman
COPY internal/deadman ./internal/deadman
COPY internal/safelog ./internal/safelog
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/halro-deadman ./cmd/halro-deadman
RUN mkdir -p /rootfs/var/lib/halro-deadman /rootfs/var/log/halro-deadman

FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35
COPY --from=build /out/halro-deadman /usr/local/bin/halro-deadman
COPY --from=build --chown=65532:65532 /rootfs/var/lib/halro-deadman/ /var/lib/halro-deadman/
COPY --from=build --chown=65532:65532 /rootfs/var/log/halro-deadman/ /var/log/halro-deadman/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/halro-deadman"]
