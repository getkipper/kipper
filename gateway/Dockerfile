FROM golang:1.26-alpine AS build

# Use the repo root as the build context to include the sibling controller
# module.
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
