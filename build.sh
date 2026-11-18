#!/bin/sh
# Runs tests, builds the arm64 (router) and amd64 (local) binaries, then the image tarball.
set -eu
cd "$(dirname "$0")"
mkdir -p build dist
go vet ./...
go vet -tags nopg ./cmd/...
go test -count=1 ./...
export CGO_ENABLED=0
# The router image has no Postgres: -tags nopg keeps the driver out of the binary.
GOOS=linux GOARCH=arm64 go build -tags nopg -trimpath -ldflags="-s -w" -o build/ispbill ./cmd/ispbill
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/ispbill-amd64 ./cmd/ispbill
# The image tool runs on the build host, so build it natively.
go build -o build/mkimage ./tools/mkimage
build/mkimage -bin build/ispbill -ca /etc/ssl/certs/ca-certificates.crt -exclude "/usr/local/share/ca-certificates/*.crt" -arch arm64 -out dist/ispbill-arm64.tar
echo "done: dist/ispbill-arm64.tar (import on the router), build/ispbill-amd64 (run locally)"
