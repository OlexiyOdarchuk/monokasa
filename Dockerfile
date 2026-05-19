# syntax=docker/dockerfile:1.7
# Multi-stage build for the monokasa backend.
#
# Stage 1 (build): pulls deps, compiles a static binary with CGO disabled.
# Stage 2 (run):   distroless, nonroot. Binary serves HTTP on :8090 and
#                  expects the SQLite file under /data (volume).
#
# When the Svelte admin/buyer UI lands (PR #4-5), a third stage will pull
# its `vite build` output and copy it next to the binary for embed.FS.

FROM golang:1.26.3-alpine AS build
WORKDIR /src

# Cache deps independently of source so editing .go files doesn't
# re-download the module graph.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/monokasa ./cmd/app

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/monokasa /monokasa
EXPOSE 8090
USER nonroot:nonroot
ENTRYPOINT ["/monokasa"]
