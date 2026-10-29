# --- Build stage ---
FROM golang:1.27.3-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src

# Cache dependencies
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download

# Copy server source
COPY server/ ./server/
COPY scripts/build-daemon-binaries.sh ./scripts/

RUN --mount=type=cache,target=/root/.cache/go-build cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/migrate ./cmd/migrate
RUN --mount=type=cache,target=/root/.cache/go-build cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/maintenance ./cmd/maintenance
RUN --mount=type=cache,target=/root/.cache/go-build cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/backfill_task_usage_hourly ./cmd/backfill_task_usage_hourly
RUN --mount=type=cache,target=/root/.cache/go-build cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/backfill_codex_usage_cache ./cmd/backfill_codex_usage_cache

# Build binaries
ARG VERSION=dev
ARG BUILD=
ARG COMMIT=unknown
ARG DATE=unknown
RUN --mount=type=cache,target=/root/.cache/go-build cd server && CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION} -X main.build=${BUILD} -X main.commit=${COMMIT}" -o bin/server ./cmd/server
# One build step for the CLI and the daemon builds served to daemons, so the
# image's own multica and the downloadable ones cannot drift apart.
RUN --mount=type=cache,target=/root/.cache/go-build VERSION="${VERSION}" BUILD="${BUILD}" COMMIT="${COMMIT}" DATE="${DATE}" sh scripts/build-daemon-binaries.sh /src/server/bin/daemon-binaries \
    && cp "server/bin/daemon-binaries/multica-linux-$(cd server && go env GOARCH)" server/bin/multica

# --- Runtime stage ---
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /src/server/bin/server .
COPY --from=builder /src/server/bin/multica .
COPY --from=builder /src/server/bin/daemon-binaries ./daemon-binaries
COPY --from=builder /src/server/bin/migrate .
COPY --from=builder /src/server/bin/maintenance .
COPY --from=builder /src/server/bin/backfill_task_usage_hourly .
COPY --from=builder /src/server/bin/backfill_codex_usage_cache .
COPY server/migrations/ ./migrations/
COPY LICENSE NOTICE ./
COPY docker/entrypoint.sh docker/entrypoint.cd.sh .
RUN sed -i 's/\r$//' entrypoint.sh entrypoint.cd.sh && chmod +x entrypoint.sh entrypoint.cd.sh

EXPOSE 8080

ENTRYPOINT ["./entrypoint.sh"]
