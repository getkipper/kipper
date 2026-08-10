FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /sidecar .

FROM scratch

COPY --from=build /sidecar /sidecar

ENTRYPOINT ["/sidecar"]
