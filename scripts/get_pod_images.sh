#!/bin/sh
# retrieves list of images from the gp_container_image label in Mimir and Loki and by querying running pods in all Kubernetes clusters
(
  (
    for context in dev-we cmb-prod-we
    do
      kubectl run curl-scrape-images -q \
        --image=curlimages/curl --rm -it --restart=Never \
        --context=$context -- \
        curl http://mimir-nginx.monitoring-mimir.svc.cluster.local/prometheus/api/v1/label/gp_container_image/values | \
        jq -r '.data[]'

      kubectl run curl-scrape-images -q \
        --image=curlimages/curl --rm -it --restart=Never \
        --context=$context -- \
        curl http://loki-querier.monitoring-loki3.svc.cluster.local:3100/loki/api/v1/label/gp_container_image/values?since=30d | \
        jq -r '.data[]'
    done
  ) && 
  (
    for context in $(kubectl config get-contexts -o name)
    do
      kubectl get pods --all-namespaces \
        -o jsonpath="{range .items[*].spec.containers[*]}{.image}{'\n'}{end}" \
        --context $context
      kubectl get scaledjobs --all-namespaces \
        -o jsonpath="{range .items[*].spec.jobTargetRef.template.spec.containers[*]}{.image}{'\n'}{end}" \
        --context $context 2> /dev/null
    done
  )
) | sort | uniq
