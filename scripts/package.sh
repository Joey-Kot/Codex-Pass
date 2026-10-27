#!/usr/bin/env bash
set -euo pipefail

if (( $# < 2 || $# > 4 )); then
  printf 'Usage: %s <linux|darwin|windows> <amd64|arm64> [version] [output-dir]\n' "$0" >&2
  exit 2
fi

target_os="$1"
target_arch="$2"
version="${3:-snapshot}"
output_dir="${4:-dist}"

case "$target_os" in
  linux|darwin|windows) ;;
  *) printf 'Unsupported operating system: %s\n' "$target_os" >&2; exit 2 ;;
esac
case "$target_arch" in
  amd64) package_arch=x86_64 ;;
  arm64) package_arch=arm64 ;;
  *) printf 'Unsupported architecture: %s\n' "$target_arch" >&2; exit 2 ;;
esac
if [[ ! "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
  printf 'Version must contain only letters, digits, dots, underscores, and hyphens.\n' >&2
  exit 2
fi

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
mkdir -p -- "$output_dir"
output_dir="$(cd -- "$output_dir" && pwd)"
staging_dir="$(mktemp -d)"
trap 'rm -rf -- "$staging_dir"' EXIT

package_name="codex-pass_${version}_${target_os}_${package_arch}"
package_os="$target_os"
if [[ "$target_os" == darwin ]]; then
  package_os=macos
fi
archive_name="codex-pass-${package_os}-${package_arch}"
package_dir="$staging_dir/$package_name"
mkdir -p -- "$package_dir"
binary_name=codex-pass
if [[ "$target_os" == windows ]]; then
  binary_name=codex-pass.exe
fi

CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
  go build -trimpath -ldflags='-s -w' -o "$package_dir/$binary_name" ./cmd/codex-pass
cp -- README.md README_ZH.md Agents.md LICENSE NOTICE prices.example.json "$package_dir/"
if [[ "$target_os" == linux ]]; then
  mkdir -p -- "$package_dir/examples"
  cp -R -- examples/systemd "$package_dir/examples/"
fi

if [[ "$target_os" == windows ]]; then
  archive_file="$archive_name.zip"
  archive="$output_dir/$archive_file"
  # Replace rather than update an older ZIP so removed files cannot linger.
  rm -f -- "$archive"
  (cd "$staging_dir" && zip -qr "$archive" "$package_name")
else
  archive_file="$archive_name.tar.gz"
  archive="$output_dir/$archive_file"
  tar -czf "$archive" -C "$staging_dir" "$package_name"
fi
(cd "$output_dir" && sha256sum "$archive_file" | tee "$archive_file.sha256")
printf 'Built %s\n' "$archive"
