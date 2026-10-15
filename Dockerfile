FROM golang:1.27.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/gostream ./cmd/gostream

FROM golang:1.27.2-bookworm
WORKDIR /app
COPY --from=build /out/gostream /usr/local/bin/gostream
COPY web ./web
RUN mkdir -p /app/data/media
ENV ADDR=:8080 MEDIA_DIR=/app/data/media DB_DRIVER=mysql
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gostream"]
