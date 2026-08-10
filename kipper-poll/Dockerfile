FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /kipper-poll .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /kipper-poll /usr/local/bin/kipper-poll
ENTRYPOINT ["kipper-poll"]
