FROM golang:latest AS builder
WORKDIR /work

ARG VERSION

COPY ./go.mod ./go.sum ./
COPY ./references/concrnt/go.mod ./references/concrnt/go.sum ./references/concrnt/
RUN go mod download && go mod verify

COPY ./ ./
RUN VERSION=${VERSION:-unknown} \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o concrnt-search .

FROM ubuntu:latest AS certificates
RUN apt-get update && apt-get install -y ca-certificates

FROM ubuntu:latest

COPY --from=builder /work/concrnt-search /usr/local/bin/concrnt-search
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

CMD ["concrnt-search"]
