FROM golang:1.25-alpine AS build

# Build context is the repo root, not gateway/, because gateway imports
# controller/pkg/hostnames (a sibling Go module). A minimal go.work references
# only the two modules this build needs; the go directive comes from the build
# image's toolchain so it always satisfies the modules' own go directives.
WORKDIR /src

COPY gateway/ ./gateway/
COPY controller/ ./controller/

RUN go work init ./gateway ./controller

WORKDIR /src/gateway
RUN go mod download

RUN CGO_ENABLED=0 go build -o /gateway .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates && \
    adduser -D -h /app kipper && \
    mkdir -p /data && \
    chown kipper:kipper /data

USER kipper
COPY --from=build /gateway /app/gateway

EXPOSE 8080

ENTRYPOINT ["/app/gateway"]
