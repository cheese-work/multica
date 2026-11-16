# --- Build stage ---
FROM golang:1.27.3-alpine AS deps

RUN apk add --no-cache git

# The builder and daemon-builder stages compile at the same time inside the 4 GB
# X99 dind (CHE-731): cap each go build to 2 compiles and ~1 GiB per process.
ENV GOFLAGS=-p=2 GOMEMLIMIT=1GiB

WORKDIR /src

# Cache dependencies
COPY server/go.mod server/go.sum ./server/
RUN --mount=type=cache,target=/go/pkg/mod cd server && go mod download

# Compile every third-party and standard-library package once, in layers that
# depend only on which packages the binaries import. The CD builder is wiped
# after each job (CHE-4314), so a cache mount never survives; a layer can be
# restored from the registry cache. Later stages start from that GOCACHE and
# recompile only the server's own code (CHE-3983).
FROM deps AS deps-list

COPY server/ ./server/
RUN --mount=type=cache,target=/go/pkg/mod cd server && mkdir /deplists \
    && pkgs="{{if not .Module}}{{.ImportPath}}{{else if not .Module.Main}}{{.ImportPath}}{{end}}" \
    && CGO_ENABLED=0 go list -deps -f "$pkgs" ./cmd/server ./cmd/migrate ./cmd/maintenance ./cmd/backfill_task_usage_hourly ./cmd/backfill_codex_usage_cache | sort -u > /deplists/server.txt \
    && for t in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do \
         CGO_ENABLED=0 GOOS="${t%/*}" GOARCH="${t#*/}" go list -deps -f "$pkgs" ./cmd/multica | sort -u > "/deplists/daemon-$(echo "$t" | tr / -).txt"; \
       done

FROM deps AS deps-compiled

# Only the package lists enter this stage, so its layer key changes with the
# import set, not with every source edit. The server cache holds the native
# build; the daemon cache holds the four cross-builds.
COPY --from=deps-list /deplists /deplists
RUN --mount=type=cache,target=/go/pkg/mod cd server && go mod download \
    && CGO_ENABLED=0 GOCACHE=/gocache/server go build $(cat /deplists/server.txt) \
    && for list in /deplists/daemon-*.txt; do \
         target="$(basename "$list" .txt)"; target="${target#daemon-}"; \
         CGO_ENABLED=0 GOOS="${target%-*}" GOARCH="${target#*-}" GOCACHE=/gocache/daemon go build $(cat "$list"); \
       done

# The server binaries and the daemon cross-builds are separate stages so
# BuildKit compiles them at the same time (CHE-3983).
FROM deps AS builder

# The server's own packages are the only code left to compile.
COPY --from=deps-compiled /gocache/server /gocache
ENV GOCACHE=/gocache

# Copy server source
COPY server/ ./server/

RUN --mount=type=cache,target=/go/pkg/mod cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/migrate ./cmd/migrate
RUN --mount=type=cache,target=/go/pkg/mod cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/maintenance ./cmd/maintenance
RUN --mount=type=cache,target=/go/pkg/mod cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/backfill_task_usage_hourly ./cmd/backfill_task_usage_hourly
RUN --mount=type=cache,target=/go/pkg/mod cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/backfill_codex_usage_cache ./cmd/backfill_codex_usage_cache

# Build binaries
ARG VERSION=dev
ARG BUILD=
ARG COMMIT=unknown
ARG DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod cd server && CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION} -X main.build=${BUILD} -X main.commit=${COMMIT}" -o bin/server ./cmd/server

FROM deps AS daemon-builder

COPY --from=deps-compiled /gocache/daemon /gocache
ENV GOCACHE=/gocache

COPY server/ ./server/
COPY scripts/build-daemon-binaries.sh ./scripts/
ARG VERSION=dev
ARG BUILD=
ARG COMMIT=unknown
ARG DATE=unknown
# One build step for the CLI and the daemon builds served to daemons, so the
# image's own multica and the downloadable ones cannot drift apart.
RUN --mount=type=cache,target=/go/pkg/mod VERSION="${VERSION}" BUILD="${BUILD}" COMMIT="${COMMIT}" DATE="${DATE}" sh scripts/build-daemon-binaries.sh /src/server/bin/daemon-binaries \
    && cp "server/bin/daemon-binaries/multica-linux-$(cd server && go env GOARCH)" server/bin/multica

# --- Runtime stage ---
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /src/server/bin/server .
COPY --from=daemon-builder /src/server/bin/multica .
COPY --from=daemon-builder /src/server/bin/daemon-binaries ./daemon-binaries
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
