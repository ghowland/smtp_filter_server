#!/bin/sh
set -eu

[ -f go.mod ] || {
    go mod init smtpfilter
    go get github.com/mileusna/spf@latest
}

[ -f cert.pem ] || {
    echo "-- generating cert.pem and key.pem"
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
        -keyout key.pem -out cert.pem \
        -subj "/CN=localhost" \
        -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" 2>/dev/null
}

go mod tidy
go vet ./... || true
CGO_ENABLED=0 go build -trimpath -o smtpfilter ./cmd/smtpfilter

