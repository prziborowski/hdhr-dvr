# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.24.4 AS build
WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=1 (default) so go-sqlite3 links; runtime is glibc.
RUN CGO_ENABLED=1 GOOS=linux \
    go build -trimpath -ldflags "-s -w" -o /out/app          ./cmd/app/ \
 && go build -trimpath -ldflags "-s -w" -o /out/guide         ./cmd/guide/ \
 && go build -trimpath -ldflags "-s -w" -o /out/auto-record   ./cmd/auto-record/ \
 && go build -trimpath -ldflags "-s -w" -o /out/convert-ts    ./cmd/convert-ts/

# ---- runtime ----
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
     ffmpeg ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/app         /usr/local/bin/app
COPY --from=build /out/guide       /usr/local/bin/guide
COPY --from=build /out/auto-record /usr/local/bin/auto-record
COPY --from=build /out/convert-ts  /usr/local/bin/convert-ts

# config.json / recordings.db are relative to CWD.
WORKDIR /app

# No ENTRYPOINT so k8s CronJobs can set `command` per job.
# Default run = the DVR server.
CMD ["/usr/local/bin/app"]
