#!/bin/sh

set -eu

VERSION="$1"
BASE_URL="$2"
SUMS="$3"
OUT="$4"

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

digest() {
    asset="$1"
    value=$(grep -E "[[:space:]][*]?${asset}\$" "$SUMS" | head -n 1 | cut -d' ' -f1 || true)
    if [ -z "$value" ]; then
        echo "render.sh: $SUMS does not list $asset" >&2
        exit 1
    fi
    printf '%s' "$value"
}

mkdir -p "$OUT"

sed \
    -e "s|__VERSION__|$VERSION|g" \
    -e "s|__BASE_URL__|$BASE_URL|g" \
    -e "s|__SHA_DARWIN_ARM64__|$(digest "zoorikctl_${VERSION}_darwin_arm64.tar.gz")|g" \
    -e "s|__SHA_DARWIN_AMD64__|$(digest "zoorikctl_${VERSION}_darwin_amd64.tar.gz")|g" \
    -e "s|__SHA_LINUX_ARM64__|$(digest "zoorikctl_${VERSION}_linux_arm64.tar.gz")|g" \
    -e "s|__SHA_LINUX_AMD64__|$(digest "zoorikctl_${VERSION}_linux_amd64.tar.gz")|g" \
    "$here/homebrew/zoorikctl.rb.tmpl" > "$OUT/zoorikctl.rb"

sed \
    -e "s|__VERSION__|$VERSION|g" \
    -e "s|__VERSION_BARE__|${VERSION#v}|g" \
    -e "s|__BASE_URL__|$BASE_URL|g" \
    -e "s|__SHA_WINDOWS_AMD64__|$(digest "zoorikctl_${VERSION}_windows_amd64.zip")|g" \
    -e "s|__SHA_WINDOWS_ARM64__|$(digest "zoorikctl_${VERSION}_windows_arm64.zip")|g" \
    "$here/scoop/zoorikctl.json.tmpl" > "$OUT/zoorikctl.json"

echo "rendered $OUT/zoorikctl.rb and $OUT/zoorikctl.json for $VERSION"
