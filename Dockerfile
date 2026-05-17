FROM golang:latest AS builder
WORKDIR /work

ARG VERSION

COPY ./go.mod ./go.sum ./
RUN go mod download && go mod verify

COPY ./ ./
RUN VERSION=${VERSION:-unknown} \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o concrnt-crawler .

FROM ubuntu:latest AS certificates
RUN apt-get update && apt-get install -y ca-certificates

FROM ubuntu:latest

COPY --from=builder /work/concrnt-crawler /usr/local/bin/concrnt-crawler
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

CMD ["concrnt-crawler"]
