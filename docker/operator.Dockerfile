FROM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 go build -mod=vendor -o /operator ./cmd/operator

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /operator /operator

USER nonroot:nonroot
ENTRYPOINT ["/operator"]
