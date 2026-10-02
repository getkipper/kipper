FROM --platform=$BUILDPLATFORM golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS build
ARG RELEASE=RELEASE.2025-09-07T16-13-09Z
RUN apk add --no-cache git
RUN git clone --depth 1 --branch "${RELEASE}" https://github.com/minio/minio.git /src
WORKDIR /src
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -tags kqueue -trimpath --ldflags "$(MINIO_RELEASE=RELEASE go run buildscripts/gen-ldflags.go)" -o /out/minio

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache ca-certificates
COPY --from=build /out/minio /usr/bin/minio
COPY --from=build /src/dockerscripts/docker-entrypoint.sh /usr/bin/docker-entrypoint.sh
COPY --from=build /src/LICENSE /src/CREDITS /licenses/minio/
ARG KIPPER_REVISION=main
COPY SOURCE.md /licenses/SOURCE.md
RUN sed -i "s/KIPPER_REVISION/${KIPPER_REVISION}/g" /licenses/SOURCE.md
RUN chmod +x /usr/bin/docker-entrypoint.sh
EXPOSE 9000
VOLUME ["/data"]
ENTRYPOINT ["/usr/bin/docker-entrypoint.sh"]
CMD ["minio"]
