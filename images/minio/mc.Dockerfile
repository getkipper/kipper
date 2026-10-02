FROM --platform=$BUILDPLATFORM golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS build
ARG RELEASE=RELEASE.2025-08-13T08-35-41Z
RUN apk add --no-cache git
RUN git clone --depth 1 --branch "${RELEASE}" https://github.com/minio/mc.git /src
WORKDIR /src
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -tags kqueue -trimpath --ldflags "$(MC_RELEASE=RELEASE go run buildscripts/gen-ldflags.go)" -o /out/mc

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache ca-certificates
COPY --from=build /out/mc /usr/bin/mc
COPY --from=build /src/LICENSE /src/CREDITS /licenses/mc/
ARG KIPPER_REVISION=main
COPY SOURCE.md /licenses/SOURCE.md
RUN sed -i "s/KIPPER_REVISION/${KIPPER_REVISION}/g" /licenses/SOURCE.md
ENTRYPOINT ["mc"]
