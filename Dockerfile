# syntax=docker/dockerfile:1

# Stage 1: build a static binary.
FROM golang:1.25 AS build
WORKDIR /src

# Download dependencies first so this layer is cached until go.mod/go.sum
# change. Avoids re-fetching the module graph on every source edit, and uses
# the committed go.sum for reproducible, verifiable builds (no `go mod tidy`
# mutating files at build time).
COPY go.mod go.sum ./
RUN go mod download

# Build.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/agent ./cmd/agent

# Stage 2: distroless runtime (no shell, minimal FS, non-root user).
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/agent /agent
ENTRYPOINT ["/agent"]
