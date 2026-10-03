#!/bin/sh
# Install the crprune binary for this machine from a GitHub release: resolve
# the release, verify its archive against the release's checksums, and replace
# DIR/crprune atomically. Messages go to stderr; the installed path to stdout.
#
#   curl -fsSL https://github.com/JohanLindvall/crprune/releases/latest/download/download.sh | sh -s -- ~/.local/bin
set -eu

repo=${CRPRUNE_REPO:-JohanLindvall/crprune}

usage() {
  echo "Usage: download.sh [-v VERSION] [DIR]"
  echo "Installs DIR/crprune (default: the current directory) from the latest"
  echo "release of github.com/$repo, or from release VERSION (e.g. v0.1.13)."
  echo "Requires curl or GNU Wget, tar, and sha256sum or shasum."
  echo "GH_TOKEN or GITHUB_TOKEN, when set, authenticates the GitHub API request."
}

die() {
  echo "download.sh: $*" >&2
  exit 1
}

# fetch URL FILE [HEADER]: save URL in FILE, failing on HTTP errors. curl
# refuses to leave HTTPS, also on redirects.
fetch() {
  url=$1 file=$2
  shift 2
  if [ "$downloader" = curl ]; then
    curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 ${1:+-H "$1"} -o "$file" "$url"
  else
    # Wget forwards custom headers to redirected hosts. An authenticated
    # API request must use the canonical URL and refuse redirects; asset
    # downloads carry no token and still follow GitHub's storage redirects.
    if [ "$#" -gt 0 ]; then
      set -- --max-redirect=0 "--header=$1"
    fi
    wget -q "$@" -O "$file" "$url"
  fi
}

# sha256 FILE: print the SHA-256 digest of FILE in hex.
sha256() {
  if [ "$hasher" = sha256sum ]; then
    sha256sum "$1"
  else
    shasum -a 256 "$1"
  fi | awk '{ print $1 }'
}

main() {
  version=
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -h|--help) usage; exit 0 ;;
      -v|--version)
        [ -n "${2:-}" ] || { usage >&2; exit 2; }
        version=$2
        shift ;;
      --) shift; break ;;
      -*) usage >&2; exit 2 ;;
      *) break ;;
    esac
    shift
  done
  case "$#" in
    0) dir=. ;;
    1) dir=$1 ;;
    *) usage >&2; exit 2 ;;
  esac

  case $(uname -s) in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) os=$(uname -s | tr '[:upper:]' '[:lower:]') ;;
  esac
  case $(uname -m) in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) arch=$(uname -m) ;;
  esac

  # BusyBox wget may not validate TLS certificates, and the checksums arrive
  # over the same connection as the archive, so only GNU Wget stands in for curl.
  if command -v curl >/dev/null 2>&1; then
    downloader=curl
  elif wget --version 2>/dev/null | head -n 1 | grep -q '^GNU Wget'; then
    downloader=wget
  else
    die "curl or GNU Wget is required"
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    hasher=sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    hasher=shasum
  else
    die "sha256sum or shasum is required"
  fi
  command -v tar >/dev/null 2>&1 || die "tar is required"

  tmp=$(mktemp -d)
  staged=
  trap 'rm -rf "$tmp" ${staged:+"$staged"}' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  case "$version" in
    ''|latest) api=https://api.github.com/repos/$repo/releases/latest ;;
    v*) api=https://api.github.com/repos/$repo/releases/tags/$version ;;
    *) api=https://api.github.com/repos/$repo/releases/tags/v$version ;;
  esac
  # The token only ever goes to the API, never to the download hosts.
  token=${GH_TOKEN:-${GITHUB_TOKEN:-}}
  if ! fetch "$api" "$tmp/release.json" ${token:+"Authorization: Bearer $token"}; then
    die "cannot read release ${version:-latest} of $repo from $api (no such release, or GitHub's API rate limit; set GH_TOKEN to raise it)"
  fi
  tag=$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/release.json" | head -n 1)
  case "$tag" in
    ''|*[!A-Za-z0-9._-]*) die "unexpected release tag \"$tag\" in $api" ;;
  esac

  assets=$(grep -o '"name": *"[^"]*"' "$tmp/release.json" | sed 's/^"name": *"\(.*\)"$/\1/')
  # Releases before v0.1.13 were named acrprune; they install under that name.
  binary=
  for name in crprune acrprune; do
    if printf '%s\n' "$assets" | grep -qxF "$name-$tag-$os-$arch.tar.gz"; then
      binary=$name
      break
    fi
  done
  if [ -z "$binary" ]; then
    available=$(printf '%s\n' "$assets" | sed -n "s/^a\{0,1\}crprune-$tag-\([^-]*\)-\([^-]*\)\.tar\.gz\$/\1\/\2/p" | tr '\n' ' ')
    available=${available% }
    die "release $tag has no build for $os/$arch (available: ${available:-none})"
  fi
  archive=$binary-$tag-$os-$arch.tar.gz
  checksums=checksums-$tag.txt
  printf '%s\n' "$assets" | grep -qxF "$checksums" || die "release $tag has no $checksums"

  base=https://github.com/$repo/releases/download/$tag
  echo "Downloading $binary $tag for $os/$arch" >&2
  fetch "$base/$archive" "$tmp/$archive" || die "cannot download $base/$archive"
  fetch "$base/$checksums" "$tmp/$checksums" || die "cannot download $base/$checksums"

  expected=$(awk -v name="$archive" '$2 == name || $2 == "*" name { print $1 }' "$tmp/$checksums")
  [ -n "$expected" ] || die "$checksums does not list $archive"
  actual=$(sha256 "$tmp/$archive")
  [ "$actual" = "$expected" ] || die "checksum mismatch for $archive: got ${actual:-nothing}, want $expected"

  tar -xzf "$tmp/$archive" -C "$tmp" "$binary" || die "cannot extract $binary from $archive"
  if [ ! -f "$tmp/$binary" ] || [ -L "$tmp/$binary" ]; then
    die "$archive holds no $binary binary"
  fi

  # Stage next to the target so that the final rename is atomic, even over a
  # binary that is running. Check it before replacing a working installation.
  case "$dir" in /*) ;; *) dir=./$dir ;; esac
  mkdir -p "$dir" || die "cannot create $dir"
  dir=$(CDPATH='' cd -- "$dir" && pwd) || die "cannot resolve $dir"
  [ ! -d "$dir/$binary" ] || die "cannot replace directory $dir/$binary"
  staged=$(mktemp "$dir/.$binary.download.XXXXXX") || die "cannot stage $dir/$binary"
  cp "$tmp/$binary" "$staged" && chmod 755 "$staged" || die "cannot stage $dir/$binary"
  "$staged" --version >&2 || die "downloaded $binary does not run on this machine; installation unchanged"
  mv -f "$staged" "$dir/$binary" ||
    die "cannot install $dir/$binary"
  staged=
  installed=$dir/$binary
  echo "$installed"
}

# Nothing runs before this last line, so a download cut short while being
# piped into sh does nothing.
main "$@"
