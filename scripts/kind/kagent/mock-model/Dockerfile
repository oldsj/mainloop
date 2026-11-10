# github.com/kagent-dev/mockllm's server, serving fixture.json behind logproxy, which logs
# the requests mockllm does not match.
FROM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS build
RUN CGO_ENABLED=0 GOBIN=/out go install github.com/kagent-dev/mockllm/cmd/mockllm@v0.1.0
COPY logproxy /src/logproxy
RUN cd /src/logproxy && CGO_ENABLED=0 go build -o /out/logproxy .

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/mockllm /mockllm
COPY --from=build /out/logproxy /logproxy
COPY fixture.json /fixture.json
ENTRYPOINT ["/logproxy"]
