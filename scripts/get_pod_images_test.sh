#!/bin/sh
set -eu
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cat > "$test_dir/kubectl" <<'MOCK'
#!/bin/sh
case "$*" in
  'config get-contexts -o name') printf 'one\ntwo\n' ;;
  *--context=two*)
    if [ "${FAIL_SECOND:-}" = 1 ]; then echo 'access denied' >&2; exit 1; fi
    echo '{"items":[]}' ;;
  *scaledjobs*) echo '{"items":[{"spec":{"jobTargetRef":{"template":{"spec":{"containers":[{"image":"reg/job:v1"}]}}}}}]}' ;;
  *) echo '{"items":[{"spec":{"containers":[{"image":"reg/app:v1"}],"initContainers":[{"image":"reg/init:v1"}],"ephemeralContainers":[{"image":"reg/debug:v1"}]},"status":{"containerStatuses":[{"image":"reg/app:v1","imageID":"docker-pullable://reg/app@sha256:pinned"}]}}]}' ;;
esac
MOCK
chmod +x "$test_dir/kubectl"
export PATH="$test_dir:$PATH"
unset MIMIR_URL LOKI_URL
sh "$script_dir/get_pod_images.sh" > "$test_dir/output"
printf 'reg/app:v1\nreg/app@sha256:pinned\nreg/debug:v1\nreg/init:v1\n' > "$test_dir/expected"
diff -u "$test_dir/expected" "$test_dir/output"
if FAIL_SECOND=1 sh "$script_dir/get_pod_images.sh" > "$test_dir/failed"; then
  echo 'A failed context was silently ignored' >&2
  exit 1
fi
test ! -s "$test_dir/failed"
sh "$script_dir/get_pod_images.sh" --scaledjobs one > "$test_dir/jobs"
grep -q '^reg/job:v1$' "$test_dir/jobs"
echo 'Image inventory tests passed'
