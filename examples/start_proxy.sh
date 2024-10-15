#
# Run proxy for Qdrant pod tenant
#

./prom-label-proxy \
   -header-name Pod \
   -label pod \
   -regex-match \
   -enable-label-apis \
   -upstream https://prometheus-monitoring.eu-central-1-0.aws.staging-cloud.qdrant.io \
   -insecure-listen-address 127.0.0.1:8080
