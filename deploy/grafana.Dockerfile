FROM grafana/grafana:13.2.1
COPY deploy/grafana/datasources/ /etc/grafana/provisioning/datasources/
COPY deploy/grafana/dashboard-providers/ /etc/grafana/provisioning/dashboards/
COPY deploy/grafana/dashboards/ /etc/grafana/gostream-dashboards/
