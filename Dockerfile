# syntax=docker/dockerfile:1
FROM golang:1.23-bookworm AS builder
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY scripts ./scripts
RUN go vet ./... && go test -race -count=1 ./... && sh scripts/build-in-container.sh

# docker build --target artifacts --output type=local,dest=./output .
FROM scratch AS artifacts
COPY --from=builder /src/output/ /

# Default image is a build/export tool, not a transparent networking appliance.
FROM golang:1.23-bookworm AS build-container
WORKDIR /src
COPY --from=builder /src /src
CMD ["sh", "-c", "cp -R /src/output/. /output/"]
