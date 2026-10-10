FROM golang:1.26-alpine AS build

# Use the repo root as the build context to include the sibling modules
# console-api and controller.
WORKDIR /src

COPY authz/ ./authz/
COPY console-api/ ./console-api/
COPY controller/ ./controller/

RUN go work init ./authz ./console-api ./controller

WORKDIR /src/authz
RUN go mod download
RUN CGO_ENABLED=0 go build -o /kipper-authz .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates && \
    adduser -D -h /app kipper

USER kipper
COPY --from=build /kipper-authz /app/kipper-authz
COPY THIRD_PARTY_NOTICES.md /app/THIRD_PARTY_NOTICES.md

EXPOSE 8080

ENTRYPOINT ["/app/kipper-authz"]
