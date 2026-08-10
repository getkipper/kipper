FROM golang:1.25-alpine AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /datamover .

FROM scratch

# The export mover dials the target cluster over public TLS, so the image
# needs a CA bundle despite being scratch-based.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /datamover /datamover

ENTRYPOINT ["/datamover"]
