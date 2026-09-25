FROM golang:1.27.1-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ ./vendor/
COPY pkg/proto/credproviderpb/ ./pkg/proto/credproviderpb/
COPY tools/round3-claude-provider/ ./tools/round3-claude-provider/

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOFLAGS=-mod=vendor

RUN mkdir -p /out && \
    go build -trimpath -ldflags='-s -w' \
      -o /out/round3-claude-provider ./tools/round3-claude-provider

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/round3-claude-provider /round3-claude-provider
USER 65532:65532
EXPOSE 50051
ENTRYPOINT ["/round3-claude-provider"]
