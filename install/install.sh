#!/bin/sh

set -eu

REPO="getzoorik/zoorikctl"
BUILD_IDENTITY='^https://github.com/getzoorik/coral/\.github/workflows/zoorikctl-release\.yaml@refs/heads/main$'
BINARY="zoorikctl"
CHECKSUM_FILE="checksums.txt"

if [ -t 2 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-}" != "dumb" ]; then
    RED=$(printf '\033[31m')
    RESET=$(printf '\033[0m')
else
    RED=''
    RESET=''
fi

say() { printf '%s\n' "[$BINARY] $*"; }
fail() {
    printf '%s\n' "${RED}[$BINARY] Error: $1${RESET}" >&2
    if [ $# -gt 1 ]; then printf '%s\n' "[$BINARY] $2" >&2; fi
    exit 1
}

need() {
    command -v "$1" >/dev/null 2>&1 || fail \
        "$1 is not installed, and this installer cannot run without it." \
        "Install $1, or download the binary yourself from https://github.com/$REPO/releases."
}

detect_os() {
    case "$(uname -s)" in
        Darwin) echo darwin ;;
        Linux)  echo linux ;;
        *) fail "zoorikctl does not ship a build for $(uname -s)." \
                "On Windows, run the PowerShell installer instead: irm https://get.zoorik.com/windows.ps1 | iex" ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  echo amd64 ;;
        arm64|aarch64) echo arm64 ;;
        *) fail "zoorikctl does not ship a build for $(uname -m)." \
                "Open an issue at https://github.com/$REPO/issues naming this architecture." ;;
    esac
}

resolve_version() {
    if [ -n "${ZOORIKCTL_VERSION:-}" ]; then
        echo "$ZOORIKCTL_VERSION"
        return
    fi
    resolved=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
        "https://github.com/$REPO/releases/latest" 2>/dev/null || true)
    resolved=${resolved##*/tag/}
    case "$resolved" in
        v*) echo "$resolved" ;;
        *) fail "Could not work out which release of zoorikctl is the latest." \
                "Set ZOORIKCTL_VERSION to a release tag from https://github.com/$REPO/releases." ;;
    esac
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | cut -d' ' -f1
    else
        fail "Neither sha256sum nor shasum is available, so the download cannot be verified." \
             "This installer will not install an unverified binary. Install coreutils, or download and check the release by hand from https://github.com/$REPO/releases."
    fi
}

verify_checksum() {
    archive="$1"
    asset="$2"
    sums="$3"

    expected=$(grep -E "[[:space:]][*]?${asset}\$" "$sums" | head -n 1 | cut -d' ' -f1 || true)
    if [ -z "$expected" ]; then
        fail "The release's $CHECKSUM_FILE does not list $asset." \
             "Nothing has been installed. This means the release is incomplete; report it at https://github.com/$REPO/issues."
    fi

    actual=$(sha256_of "$archive")
    if [ "$expected" != "$actual" ]; then
        rm -f "$archive"
        printf '%s\n' "${RED}[$BINARY] Error: Checksum mismatch. The downloaded file is not the file this release published.${RESET}" >&2
        printf '%s\n' "[$BINARY]   expected  $expected" >&2
        printf '%s\n' "[$BINARY]   actually  $actual" >&2
        printf '%s\n' "[$BINARY] Nothing has been installed and the download has been deleted. Retry once: a truncated download looks exactly like this. If it repeats, something between you and GitHub is altering the file, and you should not install it." >&2
        exit 1
    fi
    say "Checksum verified."
}

verify_signature() {
    sums="$1"
    signature="$2"
    certificate="$3"

    if ! command -v cosign >/dev/null 2>&1; then
        return 0
    fi
    if [ ! -s "$signature" ] || [ ! -s "$certificate" ]; then
        fail "The release's $CHECKSUM_FILE.sig or $CHECKSUM_FILE.pem is empty." \
             "Nothing has been installed. Do not install this download."
    fi
    say "Verifying signature..."
    if cosign verify-blob \
        --certificate "$certificate" \
        --signature "$signature" \
        --certificate-identity-regexp "$BUILD_IDENTITY" \
        --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
        "$sums" >/dev/null 2>&1; then
        say "Signature verified (keyless, built by Zoorik's release workflow)."
    else
        fail "The release's checksum file is not signed by Zoorik's release workflow." \
             "Nothing has been installed. Do not install this download."
    fi
}

install_dir() {
    if [ -n "${ZOORIKCTL_INSTALL_DIR:-}" ]; then
        echo "$ZOORIKCTL_INSTALL_DIR"
        return
    fi
    for candidate in /usr/local/bin "$HOME/.local/bin"; do
        if [ -d "$candidate" ] && [ -w "$candidate" ]; then
            echo "$candidate"
            return
        fi
    done
    mkdir -p "$HOME/.local/bin"
    echo "$HOME/.local/bin"
}

main() {
    need curl
    need tar

    os=$(detect_os)
    arch=$(detect_arch)
    if [ -z "${ZOORIKCTL_VERSION:-}" ]; then say "Resolving latest version..."; fi
    version=$(resolve_version)
    say "Version: $version"
    asset="${BINARY}_${version}_${os}_${arch}.tar.gz"
    base="https://github.com/$REPO/releases/download/$version"

    work=$(mktemp -d)
    trap 'rm -rf "$work"' EXIT INT TERM

    say "Downloading $base/$asset..."
    curl -fsSL "$base/$asset" -o "$work/$asset" || fail \
        "Could not download $asset." \
        "Check that $version is a released version at https://github.com/$REPO/releases."
    curl -fsSL "$base/$CHECKSUM_FILE" -o "$work/$CHECKSUM_FILE" || fail \
        "Could not download the release's $CHECKSUM_FILE." \
        "Nothing has been installed. This installer will not install an unverified binary."
    curl -fsSL "$base/$CHECKSUM_FILE.sig" -o "$work/$CHECKSUM_FILE.sig" || fail \
        "Could not download the release's $CHECKSUM_FILE.sig." \
        "Nothing has been installed. This installer will not install an unsigned binary."
    curl -fsSL "$base/$CHECKSUM_FILE.pem" -o "$work/$CHECKSUM_FILE.pem" || fail \
        "Could not download the release's $CHECKSUM_FILE.pem." \
        "Nothing has been installed. This installer will not install an unsigned binary."

    say "Verifying checksum..."
    verify_checksum "$work/$asset" "$asset" "$work/$CHECKSUM_FILE"
    verify_signature "$work/$CHECKSUM_FILE" "$work/$CHECKSUM_FILE.sig" "$work/$CHECKSUM_FILE.pem"

    say "Extracting..."
    tar -xzf "$work/$asset" -C "$work" || fail \
        "The downloaded archive could not be extracted." \
        "Nothing has been installed."
    [ -f "$work/$BINARY" ] || fail \
        "The archive does not contain a $BINARY binary." \
        "Nothing has been installed; report this at https://github.com/$REPO/issues."

    target=$(install_dir)
    chmod +x "$work/$BINARY"
    mv "$work/$BINARY" "$target/$BINARY" || fail \
        "Could not write $target/$BINARY." \
        "Set ZOORIKCTL_INSTALL_DIR to a directory you can write to, and add it to your PATH."

    say "$BINARY $version installed to $target/$BINARY"
    case ":$PATH:" in
        *":$target:"*)
            say "Run '$BINARY --help' to get started, or paste the connect command from the Zoorik console."
            ;;
        *)
            say "$target is not on your PATH. Add it:"
            printf '\n    export PATH="%s:$PATH"\n\n' "$target"
            say "Then run '$BINARY --help', or paste the connect command from the Zoorik console."
            ;;
    esac
}

main "$@"
