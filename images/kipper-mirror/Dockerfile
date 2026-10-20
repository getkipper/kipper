# The backup mirror image: rclone does the copying, and the kip binary runs
# the fail-closed preflight around it ('kip backup mirror run').
# Build from the repository root, because kip imports the sibling controller
# module.

FROM golang:1.26-alpine AS rclone
ARG RCLONE_VERSION=v1.75.2
ARG RCLONE_COMMIT=be8d6044eacb843b74d335db0c5fbe09222031fe
RUN apk add --no-cache git
RUN git clone --depth 1 --branch "${RCLONE_VERSION}" https://github.com/rclone/rclone.git /src
WORKDIR /src
RUN test "$(git rev-parse HEAD)" = "${RCLONE_COMMIT}"
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -X github.com/rclone/rclone/fs.Version=${RCLONE_VERSION}" \
      -o /out/rclone .
# rclone ships no attribution file, so collect the license and notice files
# of every module compiled into it.
RUN mkdir -p /out/licenses/rclone/modules && cp COPYING /out/licenses/rclone/ && \
    CGO_ENABLED=0 go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}' . | \
    sort -u | while read -r path dir; do \
      mkdir -p "/out/licenses/rclone/modules/${path}"; \
      for f in "${dir}"/LICENSE* "${dir}"/LICENCE* "${dir}"/COPYING* "${dir}"/NOTICE*; do \
        if [ -f "${f}" ]; then cp "${f}" "/out/licenses/rclone/modules/${path}/"; fi; \
      done; \
    done

FROM golang:1.26-alpine AS kip
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY kip/ ./kip/
COPY controller/ ./controller/
WORKDIR /src/kip
RUN go mod download
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/kip .

FROM scratch
COPY --from=kip /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=kip /out/kip /usr/local/bin/kip
COPY --from=rclone /out/rclone /usr/local/bin/rclone
COPY --from=rclone /out/licenses/rclone /licenses/rclone
COPY LICENSE THIRD_PARTY_NOTICES.md /licenses/kipper/
ENV PATH=/usr/local/bin HOME=/tmp
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/kip"]
