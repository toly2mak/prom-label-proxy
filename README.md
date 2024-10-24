# prom-label-proxy

The prom-label-proxy can enforce a given label in a given PromQL query, in Prometheus API responses or in Alertmanager API requests. As an example (but not only),
this allows read multi-tenancy for projects like Prometheus, Alertmanager or Thanos.

This service is a forked version of https://github.com/prometheus-community/prom-label-proxy. For general details, refer to the community docs.

We added new features and flags to align with our requirements

## How it works 

You can run `prom-label-proxy` to enforce the qdrant tenant to be injected to upstream proxy

```
LOGLEVEL=DEBUG ./prom-label-proxy \                                                
   --header-name X-Forwarded-Host \                                                
   --label pod \                                                                   
   --regex-match \                                                                 
   --value-regexp '^([a-zA-Z0-9-]+)' \                                             
   --result-fstring 'qdrant-%s.+' \                                                
   --upstream https://prometheus-monitoring.eu-central-1-0.aws.staging-cloud.qdrant.io \
   --rewrite-metrics-path '/sys_metrics' \                                         
   --metrics-cache-ttl 5 \                                                         
   --metrics-config-path examples/exposed_metrics.yaml \                           
   --insecure-listen-address 127.0.0.1:8080  
```

**header-name** - The proxy requires the client to provide a header with the Qdrant host to extract the database ID.

**label** - The label name that will be injected into all Prometheus queries.

**regex-match** - If provided, then label values will be injected as regex.

**value-regexp** - If provided, then that regex will be used to extract the value from header-name.

**result-fstring** - If provided, then the extracted value from header-name will be generated based on fstring.

**upstream** - Prometheus host.

**rewrite-metrics-path** - If provided, then the /metrics endpoint will be rewritten to a custom value.

**metrics-cache-ttl** - If provided, then the proxy uses a write-through cache with TTL for each metrics request (default 15s).

**metrics-config-path** - If provided, then the proxy will filter metric values based on the provided list (see YAML example in ./example).

**insecure-listen-address** - Proxy address.


Accessing the demo Prometheus APIs on `http://127.0.0.1:8080` will now expect
that the client's request provides the `X-Forwarded-Host` header with requested qdrant host value:

```bash
HOST=126c4e7f-1ed5-4968-a692-0c0987c75222.europe-west3-0.gcp.staging-cloud.qdrant.io

curl -v -H 'X-Forwarded-Host: '${HOST} -G 'http://127.0.0.1:8080/sys_metrics'
```
