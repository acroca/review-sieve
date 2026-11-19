# The build stage runs on the builder's native platform and cross-compiles for
# the target, so multi-platform builds do not run the Go compiler under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /review-sieve .

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/acroca/review-sieve" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /review-sieve /review-sieve
ENTRYPOINT ["/review-sieve"]
