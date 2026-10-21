FROM prom/prometheus:v3.14.0
COPY deploy/prometheus.yml /etc/prometheus/prometheus.yml
