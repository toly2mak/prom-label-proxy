#
# Run proxy for Qdrant pod tenant
#

LOGLEVEL=DEBUG ./prom-label-proxy \
   --header-name X-Forwarded-Host \
   --label pod \
   --regex-match \
   --value-regexp '^([a-zA-Z0-9-]+)' \
   --result-fstring 'qdrant-%s.+' \
   --upstream https://prometheus-monitoring.eu-central-1-0.aws.staging-cloud.qdrant.io \
   --rewrite-metrics-path '/sys_metrics' \
   --cache-ttl 5 \
   --insecure-listen-address 127.0.0.1:8080
