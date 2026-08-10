FROM golang:1.25-alpine AS build

# Build context is the repo root, not console-api/, because console-api
# imports controller/pkg/labels (a sibling Go module). The full repo
# go.work references modules we do not need at compile time (kip,
# gateway, sidecar, kipper-poll), so we write a minimal workspace that
# only references the two modules required for this build. The go
# directive is taken from the build image's toolchain, so it always
# satisfies the modules' own go directives without a hardcoded version
# to keep in sync.
WORKDIR /src

COPY console-api/ ./console-api/
COPY controller/ ./controller/

RUN go work init ./console-api ./controller

WORKDIR /src/console-api
RUN go mod download

# VERSION stamps the binary so /health reports a real build version for the
# kip CLI's version handshake. Left unset, the image reports "dev", which the
# CLI treats as "version unknown" rather than flagging a false skew.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /console-api .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates && \
    adduser -D -h /app kipper

USER kipper
COPY --from=build /console-api /app/console-api

# Ship third-party licence notices with the image. console-api links the
# MPL-2.0 go-sql-driver/mysql, so MPL-2.0 section 3.2 requires the notice and a
# source pointer to travel with the distributed binary.
COPY THIRD_PARTY_NOTICES.md /app/THIRD_PARTY_NOTICES.md

EXPOSE 8080

ENTRYPOINT ["/app/console-api"]
