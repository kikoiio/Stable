#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
version=$(tr -d '[:space:]' < "$root/VERSION")
name=stable-${version}-linux-amd64
archive_name=temporal_cli_1.9.1_linux_amd64.tar.gz
digest=09a0326a51db84d02735e53542b9ebd8c4758daf47482a9ab0abce15844e60d5
if [[ $(uname -s) != Linux || $(uname -m) != x86_64 ]]; then echo 'Linux x86_64 required' >&2; exit 1; fi
scratch=$(mktemp -d)
trap 'chmod -R u+w "$scratch" 2>/dev/null || true; rm -rf "$scratch"' EXIT
archive=${TEMPORAL_ARCHIVE:-"$scratch/$archive_name"}
if [[ ! -f "$archive" ]]; then
  curl -fL --retry 2 -o "$archive" "https://github.com/temporalio/cli/releases/download/v1.9.1/$archive_name"
fi
printf '%s  %s\n' "$digest" "$archive" | sha256sum -c -
mkdir -p "$root/dist/$name"/{bin,libexec,share,licenses}
pkg="$root/dist/$name"
cd "$root"
for app in stable agentctl agentworker; do
  output="$pkg/libexec/$app"
  if [[ $app == stable ]]; then output="$pkg/bin/$app"; fi
  GOCACHE=${GOCACHE:-"$scratch/go-cache"} GOPATH=${GOPATH:-"$scratch/gopath"} go build -buildvcs=false -trimpath -ldflags "-X main.version=$version" -o "$output" "./cmd/$app"
done
tar -xzf "$archive" -C "$scratch" temporal LICENSE
install -m 755 "$scratch/temporal" "$pkg/libexec/temporal"
install -m 644 "$scratch/LICENSE" "$pkg/licenses/temporal-LICENSE"
mkdir -p "$pkg/share"/{fixtures,schemas,workers}
cp -a fixtures/sensor_board "$pkg/share/fixtures/"
rm -f "$pkg/share/fixtures/sensor_board"/~*.lck
cp schemas/next_action.schema.json "$pkg/share/schemas/"
cp -a workers/kicad workers/computer "$pkg/share/workers/"
rm -rf "$pkg/share/workers/kicad/__pycache__"
install -m 755 scripts/install_linux.sh "$pkg/install.sh"
install -m 644 "$root/VERSION" "$pkg/VERSION"
install -m 644 README.md "$pkg/README.md"
tar -C "$root/dist" -czf "$root/dist/$name.tar.gz" "$name"
sha256sum "$root/dist/$name.tar.gz" > "$root/dist/$name.tar.gz.sha256"
printf 'Created %s\n' "$root/dist/$name.tar.gz"
