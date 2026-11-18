#!/bin/sh
# ./build.sh               tests, then the arm64 router image and the amd64 binary
# ./build.sh bundle [arch...]
#                          the server install bundle(s) for a VPS (default: amd64;
#                          arch is amd64, arm64 or all). Output:
#                          dist/ftnbilling-<version>-linux-<arch>.tar.gz + dist/SHA256SUMS
#                          This is exactly what .github/workflows/release.yml attaches
#                          to a GitHub Release. VERSION=v1.2.3 overrides the version
#                          (default: git describe).
set -eu
cd "$(dirname "$0")"
mkdir -p build dist

if [ "${1:-}" = bundle ]; then
	shift
	[ $# -gt 0 ] || set -- amd64
	[ "$1" != all ] || set -- amd64 arm64
	ver=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
	epoch=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || date +%s)}
	export CGO_ENABLED=0
	for arch in "$@"; do
		case "$arch" in amd64 | arm64) ;; *) echo "unknown arch $arch (amd64, arm64 or all)" >&2; exit 2 ;; esac
		name=ftnbilling-$ver-linux-$arch
		stage=build/bundle/$name
		[ -n "$name" ] && rm -rf "build/bundle/$name"
		mkdir -p "$stage/deploy" "$stage/docs"
		# The server binary has the Postgres driver (no nopg tag); only the router image omits it.
		GOOS=linux GOARCH=$arch go build -mod=vendor -trimpath -ldflags="-s -w" -o "$stage/ftnbilling" ./cmd/ispbill
		cp -p deploy/* "$stage/deploy/"
		cp -p docs/*.md "$stage/docs/"
		cp -p README.md "$stage/"
		printf '%s\n' "$ver" >"$stage/VERSION"
		printf '%s\n' "$arch" >"$stage/ARCH"
		printf '#!/bin/sh\n# Run from the extracted bundle: sudo DOMAIN=billing.example.co.ke ./install.sh\nexec bash "$(dirname "$0")/deploy/install.sh" "$@"\n' >"$stage/install.sh"
		chmod 755 "$stage/install.sh" "$stage/ftnbilling"
		(cd "$stage" && find . -type f ! -name SHA256SUMS -print | sed 's|^\./||' | LC_ALL=C sort | xargs sha256sum >SHA256SUMS)
		tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" -C build/bundle -cf - "$name" | gzip -n -9 >"dist/$name.tar.gz"
		echo "bundle: dist/$name.tar.gz"
	done
	(cd dist && sha256sum ftnbilling-"$ver"-linux-*.tar.gz >SHA256SUMS)
	echo "checksums: dist/SHA256SUMS"
	exit 0
fi

# Runs tests, builds the arm64 (router) and amd64 (local) binaries, then the image tarball.
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
