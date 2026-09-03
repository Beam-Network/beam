# Build from the repository root:
#
#   docker build -f actors/orchestrator/Dockerfile -t beam-orchestrator:latest .

FROM golang:1.24-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY api ./api
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 go build -o /out/beam-orchestrator ./cmd/beam-orchestrator

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=build /out/beam-orchestrator /usr/local/bin/beam-orchestrator

EXPOSE 8781 8782

ENTRYPOINT ["beam-orchestrator"]
CMD ["serve"]
