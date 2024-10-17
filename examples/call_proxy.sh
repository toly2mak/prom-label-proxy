#
# Examples curl calls
#

DBID="126c4e7f-1ed5-4968-a692-0c0987c75222"
FW_HOST_VALUE="126c4e7f-1ed5-4968-a692-0c0987c75222.europe-west3-0.gcp.staging-cloud.qdrant.io"


# Original entrypoint
# curl -v -G 'https://prometheus-monitoring.eu-central-1-0.aws.staging-cloud.qdrant.io/api/v1/query' \
#     --data-urlencode 'query=container_memory_rss{pod="qdrant-'${DBID}'-0"}'

# Proxy entrypoint
curl -v -H 'X-Forwarded-Host: '${FW_HOST_VALUE} -G 'http://127.0.0.1:8080/api/v1/query' \
    --data-urlencode 'query=container_memory_rss' 

curl -v -H 'X-Forwarded-Host: '${FW_HOST_VALUE} -G 'http://127.0.0.1:8080/sys_metrics'

# curl -v -H 'Pod: qdrant-'${DBID}'.+' -G 'http://127.0.0.1:8080/api/v1/series'

# curl -v -H 'Pod: qdrant-'${DBID}'.+' -G 'http://127.0.0.1:8080/api/v1/labels'

# curl -v -H 'Pod: qdrant-'${DBID}'.+' -G 'http://127.0.0.1:8080/api/v1/label/__name__/values'

# curl -v -H 'Pod: qdrant-'${DBID}'.+' -G 'http://127.0.0.1:8080/api/v1/label/pod/values'

# curl -v -H 'Pod: qdrant-'${DBID}'.+' -G 'http://127.0.0.1:8080/healthz'

