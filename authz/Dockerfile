FROM golang:1.25-alpine AS build

# Build context is the repo root, not authz/, because authz imports the
# CRD types from console-api (a sibling Go module), which in turn imports
# controller/pkg/labels. A minimal workspace references only the modules
# this build needs.
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
