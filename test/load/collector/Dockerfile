FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY test/load ./test/load
RUN CGO_ENABLED=0 go build -mod=vendor -o /out/collector ./test/load/collector

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/collector /collector
USER nonroot:nonroot
ENTRYPOINT ["/collector"]
