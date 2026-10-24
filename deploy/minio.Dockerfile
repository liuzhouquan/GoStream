FROM golang:1.27.2-bookworm AS build

# The MinIO community server is source-only. Pin its archived source commit so
# a clean Compose build does not depend on a removed container registry tag.
RUN GOBIN=/out go install github.com/minio/minio@7aac2a2c5b7c882e68c1ce017d8256be2feea27f

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --create-home --home-dir /data minio
COPY --from=build /out/minio /usr/local/bin/minio
USER 10001:10001
ENV HOME=/data
EXPOSE 9000 9001
ENTRYPOINT ["/usr/local/bin/minio"]
