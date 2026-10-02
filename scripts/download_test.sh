#!/bin/sh
set -eu
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
download="$script_dir/download.sh"
export MOCK_DIR="$test_dir"
web=$test_dir/web
api=$web/api.github.com/repos/JohanLindvall/crprune/releases

# The script runs with only these tools on its PATH: the real utilities it
# needs, plus mock uname and curl or wget, so a test can take curl away.
mkdir -p "$test_dir/tools" "$test_dir/curl" "$test_dir/wget" "$test_dir/busybox" "$test_dir/none"
# Scan PATH rather than use command -v, which names shell builtins and BusyBox
# applets instead of their files.
for tool in sh awk cat chmod cp env grep gzip head mkdir mktemp mv rm sed sha256sum tar tr; do
  found=
  for path_dir in $(printf '%s' "$PATH" | tr ':' ' '); do
    if [ -x "$path_dir/$tool" ]; then found=$path_dir/$tool; break; fi
  done
  [ -n "$found" ] || { echo "$tool is required" >&2; exit 1; }
  ln -s "$found" "$test_dir/tools/$tool"
done
cat > "$test_dir/none/uname" <<'MOCK'
#!/bin/sh
case "$1" in
  -s) echo "${MOCK_OS:-Linux}" ;;
  -m) echo "${MOCK_ARCH:-x86_64}" ;;
  *) echo "unexpected uname call: $*" >&2; exit 1 ;;
esac
MOCK
# Both mocks serve $MOCK_DIR/web/<host>/<path>, log every request with its
# header, refuse anything but HTTPS, and corrupt archives when CORRUPT=1.
cat > "$test_dir/curl/curl" <<'MOCK'
#!/bin/sh
out= header= url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift ;;
    -H) header=$2; shift ;;
    --proto|--proto-redir|--retry) shift ;;
    -*) ;;
    *) url=$1 ;;
  esac
  shift
done
printf 'curl %s %s\n' "$url" "$header" >> "$MOCK_DIR/calls"
case "$url" in https://*) ;; *) echo "curl: non-HTTPS URL $url" >&2; exit 1 ;; esac
file=$MOCK_DIR/web/${url#https://}
if [ ! -f "$file" ]; then
  echo 'curl: (22) The requested URL returned error: 404' >&2
  exit 22
fi
cat "$file" > "$out"
case "${CORRUPT:-}:$url" in 1:*.tar.gz) printf x >> "$out" ;; esac
MOCK
cat > "$test_dir/wget/wget" <<'MOCK'
#!/bin/sh
if [ "$1" = --version ]; then
  echo 'GNU Wget 1.24.5 built on linux-gnu.'
  exit 0
fi
out= header= url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -O) out=$2; shift ;;
    --header=*) header=${1#--header=} ;;
    -*) ;;
    *) url=$1 ;;
  esac
  shift
done
printf 'wget %s %s\n' "$url" "$header" >> "$MOCK_DIR/calls"
file=$MOCK_DIR/web/${url#https://}
if [ ! -f "$file" ]; then
  echo "wget: server returned error: HTTP/1.1 404 Not Found" >&2
  exit 8
fi
cat "$file" > "$out"
MOCK
# BusyBox wget, which may not validate certificates, has no --version.
cat > "$test_dir/busybox/wget" <<'MOCK'
#!/bin/sh
printf 'wget %s\n' "$*" >> "$MOCK_DIR/calls"
[ "$1" = --version ] || exit 1
echo 'wget: unrecognized option: version' >&2
echo 'BusyBox v1.36.1 multi-call binary.' >&2
exit 1
MOCK
chmod +x "$test_dir/none/uname" "$test_dir/curl/curl" "$test_dir/wget/wget" "$test_dir/busybox/wget"
for mocks in curl wget busybox; do cp "$test_dir/none/uname" "$test_dir/$mocks/uname"; done

# release TAG PLATFORM...: publish archives whose fake binary names its tag
# and platform, with a checksum file, as the release job does. $binary names
# the binary: releases before v0.1.13 were named acrprune.
binary=crprune
release() {
  tag=$1
  shift
  assets=$web/github.com/JohanLindvall/crprune/releases/download/$tag
  mkdir -p "$assets"
  for platform do
    stage=$test_dir/stage/$tag-$platform
    mkdir -p "$stage"
    printf '#!/bin/sh\necho "%s version %s %s"\n' "$binary" "$tag" "$platform" > "$stage/$binary"
    chmod +x "$stage/$binary"
    tar -czf "$assets/$binary-$tag-$platform.tar.gz" -C "$stage" "$binary"
  done
  (cd "$assets" && sha256sum -- *.tar.gz > "checksums-$tag.txt")
}
release v0.1.13 linux-amd64 linux-arm64
release v0.1.12 linux-amd64
mkdir -p "$api/tags"
# The API pretty-prints its JSON; the second fixture is compact, to show the
# parsing does not depend on the layout.
cat > "$api/latest" <<'JSON'
{
  "html_url": "https://github.com/JohanLindvall/crprune/releases/tag/v0.1.13",
  "tag_name": "v0.1.13",
  "name": "v0.1.13",
  "author": {"login": "JohanLindvall"},
  "assets": [
    {"name": "checksums-v0.1.13.txt", "uploader": {"login": "github-actions[bot]"}},
    {"name": "crprune-v0.1.13-linux-amd64.tar.gz"},
    {"name": "crprune-v0.1.13-linux-arm64.tar.gz"},
    {"name": "download.sh"}
  ]
}
JSON
cp "$api/latest" "$api/tags/v0.1.13"
printf '%s' '{"tag_name":"v0.1.12","name":"v0.1.12","assets":[{"name":"checksums-v0.1.12.txt"},{"name":"crprune-v0.1.12-linux-amd64.tar.gz"}]}' > "$api/tags/v0.1.12"
binary=acrprune
release v0.1.10 linux-amd64 linux-arm64
binary=crprune
printf '%s' '{"tag_name":"v0.1.10","assets":[{"name":"acrprune-v0.1.10-linux-amd64.tar.gz"},{"name":"acrprune-v0.1.10-linux-arm64.tar.gz"},{"name":"checksums-v0.1.10.txt"}]}' > "$api/tags/v0.1.10"
# A release whose checksum file does not list its archive.
release v0.1.11 linux-amd64
: > "$web/github.com/JohanLindvall/crprune/releases/download/v0.1.11/checksums-v0.1.11.txt"
printf '%s' '{"tag_name":"v0.1.11","assets":[{"name":"checksums-v0.1.11.txt"},{"name":"crprune-v0.1.11-linux-amd64.tar.gz"}]}' > "$api/tags/v0.1.11"

with_curl="$test_dir/curl:$test_dir/tools"
unset GH_TOKEN GITHUB_TOKEN CRPRUNE_REPO

# passes PATH COMMAND...: the download must succeed; stdout is in
# $test_dir/output, the requests in $test_dir/calls.
passes() {
  search=$1
  shift
  : > "$test_dir/calls"
  if ! env PATH="$search" "$@" > "$test_dir/output" 2> "$test_dir/stderr"; then
    cat "$test_dir/stderr" >&2
    echo "download failed: $*" >&2
    exit 1
  fi
}

# fails DESCRIPTION PATH COMMAND...: the download must exit non-zero. The exit
# status is left in $status, stderr in $test_dir/stderr.
fails() {
  description=$1
  search=$2
  shift 2
  : > "$test_dir/calls"
  status=0
  env PATH="$search" "$@" > "$test_dir/output" 2> "$test_dir/stderr" || status=$?
  if [ "$status" -eq 0 ]; then
    echo "$description: the download succeeded" >&2
    exit 1
  fi
}

# installed DIR WANT: DIR/crprune runs and prints WANT, and nothing else was
# left in DIR.
installed() {
  if [ "$("$1/crprune" --version)" != "$2" ]; then
    echo "$1/crprune: got '$("$1/crprune" --version)', want '$2'" >&2
    exit 1
  fi
  if [ "$(ls -A "$1")" != crprune ]; then
    echo "$1 holds more than crprune: $(ls -A "$1")" >&2
    exit 1
  fi
}

# untouched DESCRIPTION: the last run made no request and installed nothing.
untouched() {
  if [ -s "$test_dir/calls" ] || [ -e "$test_dir/cut" ]; then
    echo "$1: the script made requests or installed something" >&2
    exit 1
  fi
}

# The latest release, into a directory that does not exist yet. stdout is the
# installed path; the API request carries no credentials without a token.
passes "$with_curl" sh "$download" "$test_dir/out/bin"
installed "$test_dir/out/bin" 'crprune version v0.1.13 linux-amd64'
[ "$(cat "$test_dir/output")" = "$test_dir/out/bin/crprune" ]
grep -qx 'curl https://api.github.com/repos/JohanLindvall/crprune/releases/latest ' "$test_dir/calls"
grep -qx 'curl https://github.com/JohanLindvall/crprune/releases/download/v0.1.13/crprune-v0.1.13-linux-amd64.tar.gz ' "$test_dir/calls"
grep -qx 'curl https://github.com/JohanLindvall/crprune/releases/download/v0.1.13/checksums-v0.1.13.txt ' "$test_dir/calls"

# Piped into sh, as the README's one-liner does. A download cut short does
# nothing: everything runs from the script's last line.
passes "$with_curl" sh -s -- "$test_dir/piped" < "$download"
installed "$test_dir/piped" 'crprune version v0.1.13 linux-amd64'
sed '$d' "$download" > "$test_dir/cut.sh"
passes "$with_curl" sh -s -- "$test_dir/cut" < "$test_dir/cut.sh"
untouched 'download cut before its last line'
sed '/mkdir -p "$dir"/q' "$download" > "$test_dir/cut.sh"
fails 'download cut inside main' "$with_curl" sh -s -- "$test_dir/cut" < "$test_dir/cut.sh"
untouched 'download cut inside main'

# A given version, with or without its v, replaces the installed binary.
passes "$with_curl" sh "$download" -v 0.1.12 "$test_dir/out/bin"
installed "$test_dir/out/bin" 'crprune version v0.1.12 linux-amd64'
passes "$with_curl" sh "$download" --version v0.1.13 -- "$test_dir/out/bin"
installed "$test_dir/out/bin" 'crprune version v0.1.13 linux-amd64'
passes "$with_curl" sh "$download" -v latest "$test_dir/out/bin"
grep -q '/releases/latest ' "$test_dir/calls"

# A release from before the rename installs under its own name, acrprune,
# next to (not over) crprune.
passes "$with_curl" sh "$download" -v v0.1.10 "$test_dir/out/bin"
[ "$(cat "$test_dir/output")" = "$test_dir/out/bin/acrprune" ]
[ "$("$test_dir/out/bin/acrprune" --version)" = 'acrprune version v0.1.10 linux-amd64' ]
[ "$("$test_dir/out/bin/crprune" --version)" = 'crprune version v0.1.13 linux-amd64' ]
rm "$test_dir/out/bin/acrprune"

# The default directory is the current one, and arm64 machines get arm64.
mkdir "$test_dir/cwd"
(cd "$test_dir/cwd" && passes "$with_curl" env MOCK_ARCH=aarch64 sh "$download")
installed "$test_dir/cwd" 'crprune version v0.1.13 linux-arm64'

# A token authenticates the API request only, never the downloads.
passes "$with_curl" env GH_TOKEN=secret sh "$download" "$test_dir/token"
grep -qx 'curl https://api.github.com/repos/JohanLindvall/crprune/releases/latest Authorization: Bearer secret' "$test_dir/calls"
if grep -v api.github.com "$test_dir/calls" | grep -q secret; then
  echo 'the token was sent to a download host' >&2
  exit 1
fi
passes "$with_curl" env GITHUB_TOKEN=other sh "$download" "$test_dir/token"
grep -q 'Authorization: Bearer other' "$test_dir/calls"

# wget works when curl is missing.
passes "$test_dir/wget:$test_dir/tools" env GH_TOKEN=secret sh "$download" "$test_dir/wget-out"
installed "$test_dir/wget-out" 'crprune version v0.1.13 linux-amd64'
grep -qx 'wget https://api.github.com/repos/JohanLindvall/crprune/releases/latest Authorization: Bearer secret' "$test_dir/calls"
fails 'no downloader' "$test_dir/none:$test_dir/tools" sh "$download" "$test_dir/none-out"
grep -q 'curl or GNU Wget is required' "$test_dir/stderr"
fails 'BusyBox wget' "$test_dir/busybox:$test_dir/tools" sh "$download" "$test_dir/none-out"
grep -q 'curl or GNU Wget is required' "$test_dir/stderr"
if grep -v -- '--version' "$test_dir/calls" | grep -q .; then
  echo 'BusyBox wget was used for a download' >&2
  exit 1
fi
[ ! -e "$test_dir/none-out" ]

# Failures install nothing and leave an existing binary alone.
mkdir "$test_dir/keep"
printf '#!/bin/sh\necho old\n' > "$test_dir/keep/crprune"
chmod +x "$test_dir/keep/crprune"
fails 'corrupt archive' "$with_curl" env CORRUPT=1 sh "$download" "$test_dir/keep"
grep -q 'checksum mismatch for crprune-v0.1.13-linux-amd64.tar.gz' "$test_dir/stderr"
installed "$test_dir/keep" old
fails 'unlisted archive' "$with_curl" sh "$download" -v v0.1.11 "$test_dir/keep"
grep -q 'checksums-v0.1.11.txt does not list crprune-v0.1.11-linux-amd64.tar.gz' "$test_dir/stderr"
installed "$test_dir/keep" old
fails 'unknown release' "$with_curl" sh "$download" -v v9.9.9 "$test_dir/keep"
grep -q 'cannot read release v9.9.9 of JohanLindvall/crprune' "$test_dir/stderr"
installed "$test_dir/keep" old
fails 'no build for the platform' "$with_curl" env MOCK_OS=Darwin MOCK_ARCH=arm64 sh "$download" "$test_dir/keep"
grep -q 'release v0.1.13 has no build for darwin/arm64 (available: linux/amd64 linux/arm64)' "$test_dir/stderr"
installed "$test_dir/keep" old
if grep -q 'releases/download' "$test_dir/calls"; then
  echo 'an archive was downloaded for a platform without a build' >&2
  exit 1
fi

for arguments in '-x' '--version' 'one two' 'one -v v0.1.13'; do
  # shellcheck disable=SC2086 # split the arguments on purpose
  fails "arguments $arguments" "$with_curl" sh "$download" $arguments
  if [ "$status" -ne 2 ] || [ -s "$test_dir/calls" ]; then
    echo "arguments $arguments: exit status $status, want 2 before any request" >&2
    exit 1
  fi
done
for help in -h --help; do
  passes "$with_curl" sh "$download" "$help"
  grep -q '^Usage: ' "$test_dir/output"
done
echo 'Download script tests passed'
