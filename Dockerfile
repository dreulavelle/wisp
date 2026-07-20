# The build stage is pinned to the *builder's* platform and cross-compiles via
# GOOS/GOARCH. wisp is CGO_ENABLED=0, so it needs no target toolchain — and this
# keeps the Go compile off QEMU entirely. Emulating the arm64 build was the bulk
# of CI image time; only the tiny runtime stage below is emulated now.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Cache mounts make local rebuilds incremental. They are not exported to the
# GHA cache, so CI still pays a cold compile — the CI win is the cross-compile
# above, not these.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -o /wisp ./cmd/wisp

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /wisp /usr/local/bin/wisp
EXPOSE 8080
ENTRYPOINT ["wisp"]
