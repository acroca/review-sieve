FROM golang:1.27.3-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /review-sieve .

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/acroca/review-sieve" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /review-sieve /review-sieve
ENTRYPOINT ["/review-sieve"]
