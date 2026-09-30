#!/bin/sh
set -eu
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
inventory="$script_dir/get_pod_images.sh"
export MOCK_DIR="$test_dir"

# The mocks log every call and answer from the fixtures below. Environment
# variables make individual queries fail.
cat > "$test_dir/kubectl" <<'MOCK'
#!/bin/sh
printf '%s\n' "$*" >> "$MOCK_DIR/calls"
case "$*" in
  'config get-contexts -o name')
    [ "${NO_CONTEXTS:-}" = 1 ] || printf 'one\ntwo\n'
    exit 0 ;;
esac
if [ "${READ_STDIN:-}" = 1 ]; then cat >> "$MOCK_DIR/stdin"; fi
case "$*" in
  *--context=two*)
    if [ "${FAIL_SECOND:-}" = 1 ]; then echo 'access denied' >&2; exit 1; fi
    echo '{"items":[]}' ;;
  *' get pods '*) cat "$MOCK_DIR/pods.json" ;;
  *' get deployments.apps,'*)
    if [ "${FORBID_WORKLOADS:-}" = 1 ]; then echo 'Error from server (Forbidden): deployments.apps is forbidden' >&2; exit 1; fi
    cat "$MOCK_DIR/workloads.json" ;;
  *' get scaledjobs.keda.sh '*)
    if [ "${FAIL_SCALEDJOBS:-}" = 1 ]; then echo 'error: the server doesn'\''t have a resource type "scaledjobs"' >&2; exit 1; fi
    echo '{"items":[{"spec":{"jobTargetRef":{"template":{"spec":{"containers":[{"image":"reg/job:v1"}]}}}}}]}' ;;
  *) echo "unexpected kubectl call: $*" >&2; exit 1 ;;
esac
MOCK
cat > "$test_dir/curl" <<'MOCK'
#!/bin/sh
for url do :; done
case "$url" in
  */mimir-ok) echo '{"status":"success","data":["reg/mimir:v1",42]}' ;;
  */loki-ok) echo '{"status":"success","data":["reg/loki:v1"]}' ;;
  */http-error) echo 'curl: (22) The requested URL returned error: 500' >&2; exit 22 ;;
  */error-status) echo '{"status":"error","data":["reg/partial:v1"]}' ;;
  */not-array) echo '{"status":"success","data":{"image":"reg/partial:v1"}}' ;;
  *) echo "unexpected curl call: $*" >&2; exit 1 ;;
esac
MOCK
chmod +x "$test_dir/kubectl" "$test_dir/curl"
cat > "$test_dir/pods.json" <<'JSON'
{"items": [
  {"spec": {"containers": [{"image": "reg/app:v1"}], "initContainers": [{"image": "reg/init:v1"}],
            "ephemeralContainers": [{"image": "reg/debug:v1"}]},
   "status": {"containerStatuses": [{"image": "reg/app:v1", "imageID": "docker-pullable://reg/app@sha256:pinned"}]}},
  {"spec": {"containers": [{"image": "reg/worker:v1"}]},
   "status": {"containerStatuses": [{"image": "reg/worker:v1", "imageID": "containerd://reg/worker@sha256:running"}]}}
]}
JSON
# As kubectl returns several resource types: one List with every object.
cat > "$test_dir/workloads.json" <<'JSON'
{"kind": "List", "items": [
  {"kind": "Deployment", "spec": {"replicas": 0, "template": {"spec": {
    "containers": [{"image": "reg/idle:v1"}], "initContainers": [{"image": "reg/idle-init:v1"}]}}}},
  {"kind": "Deployment", "spec": {"replicas": 1, "template": {"spec": {"containers": [{"image": "reg/app:v1"}]}}}},
  {"kind": "StatefulSet", "spec": {"replicas": 0, "template": {"spec": {"containers": [{"image": "reg/db:v2"}]}}}},
  {"kind": "DaemonSet", "spec": {"template": {"spec": {"containers": [{"image": ""}, {"name": "no-image"}]}}}},
  {"kind": "ReplicaSet", "spec": {"replicas": 0, "template": {"spec": {"containers": [{"image": "reg/app:v0"}]}}}},
  {"kind": "ReplicationController", "spec": {"replicas": 0, "template": {"spec": {"containers": [{"image": "reg/legacy:v1"}]}}}},
  {"kind": "Job", "spec": {"template": {"spec": {"containers": [{"image": "reg/migrate:v3"}]}}}},
  {"kind": "CronJob", "spec": {"jobTemplate": {"spec": {"template": {"spec": {"containers": [{"image": "reg/cron:v1"}]}}}}}}
]}
JSON
export PATH="$test_dir:$PATH"
unset MIMIR_URL LOKI_URL

# passes COMMAND...: the inventory must succeed; its output is in $test_dir/output.
passes() {
  : > "$test_dir/calls"
  if ! "$@" > "$test_dir/output" 2> "$test_dir/stderr"; then
    cat "$test_dir/stderr" >&2
    echo "Inventory failed: $*" >&2
    exit 1
  fi
}

# fails DESCRIPTION COMMAND...: the inventory must exit non-zero without
# printing anything, so a pipeline never sees a partial list. The exit status
# is left in $status.
fails() {
  description=$1
  shift
  : > "$test_dir/calls"
  status=0
  "$@" > "$test_dir/failed" 2> "$test_dir/stderr" || status=$?
  if [ "$status" -eq 0 ]; then
    echo "$description: the inventory succeeded" >&2
    exit 1
  fi
  if [ -s "$test_dir/failed" ]; then
    echo "$description: a partial inventory was printed" >&2
    exit 1
  fi
}

# Every kubeconfig context, with pods and workload templates: the zero-replica
# Deployment, the CronJob and the rollback ReplicaSet have no pod.
passes sh "$inventory"
cat > "$test_dir/expected" <<'EOF'
reg/app:v0
reg/app:v1
reg/app@sha256:pinned
reg/cron:v1
reg/db:v2
reg/debug:v1
reg/idle-init:v1
reg/idle:v1
reg/init:v1
reg/legacy:v1
reg/migrate:v3
reg/worker:v1
reg/worker@sha256:running
EOF
diff -u "$test_dir/expected" "$test_dir/output"
grep -q -- '--context=two' "$test_dir/calls"

# --pods-only never lists workloads, so it works without that permission.
passes env FORBID_WORKLOADS=1 sh "$inventory" --pods-only
printf 'reg/app:v1\nreg/app@sha256:pinned\nreg/debug:v1\nreg/init:v1\nreg/worker:v1\nreg/worker@sha256:running\n' > "$test_dir/expected"
diff -u "$test_dir/expected" "$test_dir/output"
if grep -q deployments "$test_dir/calls"; then
  echo '--pods-only listed workloads' >&2
  exit 1
fi
fails 'forbidden workload list' env FORBID_WORKLOADS=1 sh "$inventory"
fails 'failed second context' env FAIL_SECOND=1 sh "$inventory"

passes sh "$inventory" --pods-only --scaledjobs one
grep -qx 'reg/job:v1' "$test_dir/output"
passes sh "$inventory" --scaledjobs --pods-only one
grep -qx 'reg/job:v1' "$test_dir/output"
fails 'failed scaledjobs query' env FAIL_SCALEDJOBS=1 sh "$inventory" --scaledjobs one

fails 'no contexts' env NO_CONTEXTS=1 sh "$inventory"
grep -q 'No Kubernetes contexts found' "$test_dir/stderr"

for arguments in '-bad' '--unknown one' 'one --scaledjobs'; do
  # shellcheck disable=SC2086 # split the arguments on purpose
  fails "arguments $arguments" sh "$inventory" $arguments
  if [ "$status" -ne 2 ] || [ -s "$test_dir/calls" ]; then
    echo "arguments $arguments: exit status $status, want 2 before any kubectl call" >&2
    exit 1
  fi
done
for help in -h --help; do
  passes sh "$inventory" "$help"
  grep -q '^Usage: ' "$test_dir/output"
done

# kubectl keeps the caller's stdin (credential plugins prompt on it), and
# reading it does not consume the remaining contexts.
: > "$test_dir/stdin"
printf 'caller input\n' | passes env READ_STDIN=1 sh "$inventory" --pods-only
if [ "$(cat "$test_dir/stdin")" != 'caller input' ] || ! grep -q -- '--context=two' "$test_dir/calls"; then
  echo 'kubectl did not get the caller stdin, or a context was skipped' >&2
  exit 1
fi

MIMIR_OK=http://mimir.test/mimir-ok
LOKI_OK=http://loki.test/loki-ok
passes env MIMIR_URL="$MIMIR_OK" LOKI_URL="$LOKI_OK" sh "$inventory" --pods-only one
grep -qx 'reg/mimir:v1' "$test_dir/output"
grep -qx 'reg/loki:v1' "$test_dir/output"
if grep -qx 42 "$test_dir/output"; then
  echo 'a non-string metrics value was included' >&2
  exit 1
fi
for failure in http-error error-status not-array; do
  fails "Mimir $failure" env MIMIR_URL="http://mimir.test/$failure" sh "$inventory" --pods-only one
  fails "Loki $failure" env MIMIR_URL="$MIMIR_OK" LOKI_URL="http://loki.test/$failure" sh "$inventory" --pods-only one
done
echo 'Image inventory tests passed'
