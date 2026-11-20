# For people with docker buildx. The primary artifact is dist/ispbill-arm64.tar
# made by tools/mkimage (no Docker needed).
#
#   docker buildx build --platform linux/arm64 -t ispbill:latest --output type=docker,dest=ispbill-arm64.tar .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
# -tags nopg: the router image uses the JSON file, so the Postgres driver is left out.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -tags nopg -trimpath -ldflags="-s -w" -o /out/ispbill ./cmd/ispbill
RUN mkdir -p /out/data /out/tmp && chmod 1777 /out/tmp

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ispbill /ispbill
COPY --from=build /out/data /data
COPY --from=build --chmod=1777 /out/tmp /tmp
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DATA_DIR=/data LISTEN_ADDR=:8080
EXPOSE 8080
WORKDIR /
ENTRYPOINT ["/ispbill"]
