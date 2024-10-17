#!/bin/bash

helm package helm/prom-label-proxy -d helm-releases/

helm repo index --merge index.yaml .
