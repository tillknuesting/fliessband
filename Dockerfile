# Fliessband needs the Go toolchain at run time: it builds and tests the services
# it generates. The image carries the pinned tools and runs as a non-root user.
#
#   docker build -t fliessband .
#   docker run -p 7070:7070 -e FLIESSBAND_TOKEN=$(openssl rand -hex 16) -v fliessband-data:/data fliessband
#
# Mutation testing, fuzzing and the tests all run inside the container. The
# PostgreSQL integration station needs a Docker daemon and is skipped here
# unless you give the container access to one.
FROM golang:1.27 AS tools
ARG BUF_VERSION=v1.75.0
ARG PROTOC_GEN_GO_VERSION=v1.36.12
ARG PROTOC_GEN_GRPC_VERSION=v1.6.2
ARG GOLANGCI_LINT_VERSION=v2.15.0
ARG GOVULNCHECK_VERSION=v1.10.0
RUN GOBIN=/tools go install github.com/bufbuild/buf/cmd/buf@${BUF_VERSION} && \
    GOBIN=/tools go install google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION} && \
    GOBIN=/tools go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GRPC_VERSION} && \
    GOBIN=/tools go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION} && \
    GOBIN=/tools go install golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fliessband ./cmd/fliessband

FROM golang:1.27
RUN useradd --create-home --uid 10001 fliessband && mkdir -p /data && chown fliessband /data
COPY --from=tools /tools /opt/fliessband/tools
COPY --from=build /out/fliessband /usr/local/bin/fliessband
COPY specs /opt/fliessband/specs
USER fliessband
WORKDIR /opt/fliessband
ENV FLIESSBAND_TOOLS=/opt/fliessband/tools FLIESSBAND_DATA=/data FLIESSBAND_ADDR=0.0.0.0:7070
EXPOSE 7070
HEALTHCHECK --interval=30s --timeout=3s CMD ["curl", "-fsS", "http://127.0.0.1:7070/healthz"]
ENTRYPOINT ["fliessband", "serve"]
