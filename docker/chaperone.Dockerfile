FROM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 go build -mod=vendor -o /sidecar ./cmd/sidecar

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /sidecar /sidecar

USER nonroot:nonroot
ENTRYPOINT ["/sidecar"]
