#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
version=$(tr -d '[:space:]' < "$root/VERSION")
[[ $version =~ ^[0-9A-Za-z][0-9A-Za-z._+-]*$ ]] || { echo 'Invalid VERSION' >&2; exit 1; }
[[ $(uname -s) == Darwin ]] || { echo 'macOS required' >&2; exit 1; }

case $(uname -m) in
  x86_64)
    arch=amd64
    archive_name=temporal_cli_1.9.1_darwin_amd64.tar.gz
    digest=48cdd6c84c56e27ae8e4d9a47052b89289a9c552dee029b49dd312c6fb72873f
    ;;
  arm64)
    arch=arm64
    archive_name=temporal_cli_1.9.1_darwin_arm64.tar.gz
    digest=41e0425378fcb4fb5766340b97435e20fe47bbff2d7bf644ec2d51f7662b7c56
    ;;
  *) echo "Unsupported macOS architecture: $(uname -m)" >&2; exit 1 ;;
esac

name=stable-${version}-macos-${arch}
mkdir -p "$root/run" "$root/dist"
scratch=$(mktemp -d "$root/run/.package-macos-XXXXXXXX")
trap 'chmod -R u+w "$scratch" 2>/dev/null || true; rm -rf "$scratch"' EXIT
archive=${TEMPORAL_ARCHIVE:-"$scratch/$archive_name"}
if [[ ! -f $archive ]]; then
  curl -fL --retry 2 -o "$archive" "https://github.com/temporalio/cli/releases/download/v1.9.1/$archive_name"
fi
printf '%s  %s\n' "$digest" "$archive" | shasum -a 256 -c -

pkg="$root/dist/$name"
rm -rf -- "$pkg"
mkdir -p "$pkg"/{bin,libexec,share,licenses}
cd "$root"
for app in stable agentctl agentworker; do
  output="$pkg/libexec/$app"
  [[ $app != stable ]] || output="$pkg/bin/$app"
  CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" go build -p 2 -buildvcs=false -trimpath -ldflags "-X main.version=$version" -o "$output" "./cmd/$app"
done
tar -xzf "$archive" -C "$scratch" temporal LICENSE
install -m 755 "$scratch/temporal" "$pkg/libexec/temporal"
install -m 644 "$scratch/LICENSE" "$pkg/licenses/temporal-LICENSE"
mkdir -p "$pkg/share"/{fixtures,schemas,workers}
cp -a fixtures/sensor_board "$pkg/share/fixtures/"
rm -f "$pkg/share/fixtures/sensor_board"/~*.lck
cp schemas/next_action.schema.json schemas/criteria_proposal.schema.json "$pkg/share/schemas/"
cp -a workers/kicad workers/computer "$pkg/share/workers/"
rm -rf "$pkg/share/workers/kicad/__pycache__"
install -m 755 scripts/install_macos.sh "$pkg/install.sh"
install -m 644 "$root/VERSION" "$pkg/VERSION"
tar -C "$root/dist" -czf "$root/dist/$name.tar.gz" "$name"
(cd "$root/dist" && shasum -a 256 "$name.tar.gz" > "$name.tar.gz.sha256")
printf 'Created %s\n' "$root/dist/$name.tar.gz"
