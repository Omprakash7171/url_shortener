# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.27-alpine AS build
WORKDIR /src

# Cache module downloads first (dependencies change less often than source).
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/worker ./cmd/worker

# ---- runtime stage ----
FROM alpine:3.21

RUN addgroup -S app && adduser -S -G app app

WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /out/worker /app/worker

USER app
EXPOSE 8080

ENTRYPOINT ["/app/server"]