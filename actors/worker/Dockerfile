# Build from the repository root:
#
#   docker build -f actors/worker/Dockerfile -t beam-worker:latest .

FROM golang:1.24-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY api ./api
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 go build -o /out/beam-worker ./cmd/beam-worker

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=build /out/beam-worker /usr/local/bin/beam-worker

ENTRYPOINT ["beam-worker"]
CMD ["serve", "--capabilities", "transfer.multipart,room.transfer"]
