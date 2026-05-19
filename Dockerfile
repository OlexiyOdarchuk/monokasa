# syntax=docker/dockerfile:1.7
# One-shot build: Svelte SPA → Go binary with the SPA embedded → distroless.
# Produces a single self-contained image that hosts everything on :8093.

# Stage 1 — build the Svelte SPA into static files.
FROM node:24-alpine AS frontend
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm install
COPY frontend/ ./
RUN npm run build

# Stage 2 — build the Go binary with the SPA baked in via embed.FS.
FROM golang:1.26.3-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY . .
# Replace the in-repo stub dist (which exists only so local `go build` doesn't
# fail on an empty embed) with the real Svelte output before compiling.
RUN rm -rf /src/internal/webui/dist
COPY --from=frontend /app/build /src/internal/webui/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/monokasa ./cmd/app

# Stage 3 — distroless runtime, nonroot. Nothing else to ship.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=backend /out/monokasa /monokasa
EXPOSE 8093
USER nonroot:nonroot
ENTRYPOINT ["/monokasa"]
