#!/bin/sh
# Emit a complete image inventory, or fail without emitting a partial one.
set -eu

usage() {
  echo "Usage: $0 [--scaledjobs] [context ...]"
  echo "Defaults to all kubeconfig contexts. Requires kubectl and jq."
  echo "Includes normal, init and ephemeral containers, plus running image digests."
  echo "Optional MIMIR_URL and LOKI_URL are full image-label API URLs reachable locally."
}

scaledjobs=false
case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  --scaledjobs) scaledjobs=true; shift ;;
esac
for context in "$@"; do
  case "$context" in -*) usage >&2; exit 2 ;; esac
done
for command in kubectl jq; do
  command -v "$command" >/dev/null 2>&1 || { echo "$command is required" >&2; exit 1; }
done

inventory_dir=$(mktemp -d)
trap 'rm -rf "$inventory_dir"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
: > "$inventory_dir/images"
if [ "$#" -gt 0 ]; then
  printf '%s\n' "$@" > "$inventory_dir/contexts"
else
  kubectl config get-contexts -o name > "$inventory_dir/contexts"
fi
if [ ! -s "$inventory_dir/contexts" ]; then
  echo "No Kubernetes contexts found" >&2
  exit 1
fi

while IFS= read -r context; do
  [ -n "$context" ] || continue
  echo "Collecting images from $context" >&2
  kubectl --context="$context" --request-timeout="${KUBECTL_REQUEST_TIMEOUT:-60s}" \
    get pods --all-namespaces -o json > "$inventory_dir/pods.json"
  jq -r '.items[] | (
    .spec.containers[]?.image,
    .spec.initContainers[]?.image,
    .spec.ephemeralContainers[]?.image,
    .status.containerStatuses[]?.image,
    .status.containerStatuses[]?.imageID,
    .status.initContainerStatuses[]?.image,
    .status.initContainerStatuses[]?.imageID,
    .status.ephemeralContainerStatuses[]?.image,
    .status.ephemeralContainerStatuses[]?.imageID
  ) | select(type == "string" and length > 0) | sub("^[a-zA-Z][a-zA-Z0-9+.-]*://"; "")' \
    "$inventory_dir/pods.json" >> "$inventory_dir/images"
  if [ "$scaledjobs" = true ]; then
    kubectl --context="$context" --request-timeout="${KUBECTL_REQUEST_TIMEOUT:-60s}" \
      get scaledjobs.keda.sh --all-namespaces -o json > "$inventory_dir/jobs.json"
    jq -r '.items[] | .spec.jobTargetRef.template.spec | (.containers[]?.image, .initContainers[]?.image) | select(type == "string")' \
      "$inventory_dir/jobs.json" >> "$inventory_dir/images"
  fi
done < "$inventory_dir/contexts"

# Metrics endpoints are explicitly configured; no hard-coded clusters and no
# temporary Kubernetes pods. Check both HTTP and API success before publishing.
for metrics_url in "${MIMIR_URL:-}" "${LOKI_URL:-}"; do
  [ -n "$metrics_url" ] || continue
  curl --fail --silent --show-error --max-time 60 "$metrics_url" > "$inventory_dir/metrics.json"
  jq -e '.status == "success" and (.data | type == "array")' "$inventory_dir/metrics.json" >/dev/null
  jq -r '.data[] | select(type == "string")' "$inventory_dir/metrics.json" >> "$inventory_dir/images"
done

LC_ALL=C sort -u "$inventory_dir/images"
