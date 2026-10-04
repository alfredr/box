# The Docker CLI runtime supplies the Compose plugin used by the server.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/box ./cmd/box

FROM docker:29-cli
COPY --from=build /out/box /usr/local/bin/box
ENTRYPOINT ["box"]
CMD ["serve"]
