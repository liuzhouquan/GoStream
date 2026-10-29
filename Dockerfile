FROM golang:1.27.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/gostream ./cmd/gostream \
    && CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/gostream-media ./cmd/gostream-media \
    && CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/gostream-events ./cmd/gostream-events

FROM golang:1.27.2-bookworm
WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/gostream /usr/local/bin/gostream
COPY --from=build /out/gostream-media /usr/local/bin/gostream-media
COPY --from=build /out/gostream-events /usr/local/bin/gostream-events
COPY web ./web
COPY third_party_licenses ./third_party_licenses
RUN mkdir -p /app/data/media
ENV ADDR=:8080 MEDIA_DIR=/app/data/media DB_DRIVER=mysql
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gostream"]
