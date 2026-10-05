#!/usr/bin/env bash
# Platform boundary gate for S01: business packages must not call platform APIs
# directly. Allowed: internal/platform/**, files with linux/!linux build tags,
# and portable signal constants SIGTERM/SIGINT.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

fail=0
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Collect .go files under internal/ and cmd/, excluding platform adapters and
# files whose first build constraint is linux or !linux.
mapfile -t files < <(find internal cmd -name '*.go' -print | sort)
for f in "${files[@]}"; do
	case "$f" in
	internal/platform/*) continue ;;
	esac
	head="$(head -n 8 "$f")"
	if grep -qE '^//go:build (linux|!linux)\b' <<<"$head"; then
		continue
	fi
	# unix import / runtime.GOOS / syscall. usage
	if grep -nE 'golang.org/x/sys/unix|runtime\.GOOS' "$f" >"$tmp/hits"; then
		echo "platform API in $f:"
		cat "$tmp/hits"
		fail=1
	fi
	if grep -nE 'syscall\.' "$f" >"$tmp/hits"; then
		# allow portable signal constants only
		if grep -vE 'syscall\.SIG(TERM|INT)\b' "$tmp/hits" >"$tmp/bad" && [[ -s "$tmp/bad" ]]; then
			echo "syscall use in $f:"
			cat "$tmp/bad"
			fail=1
		fi
	fi
done

# LinuxManager{} constructions only allowed under internal/platform/sandbox.
if grep -Rn --include='*.go' 'LinuxManager{}' . | grep -v 'internal/platform/sandbox/' >"$tmp/ctors"; then
	echo "LinuxManager{} outside platform/sandbox:"
	cat "$tmp/ctors"
	fail=1
fi

echo "GOOS=windows go build ./..."
if ! CGO_ENABLED=0 GOOS=windows go build ./...; then
	echo "windows build failed"
	fail=1
fi
echo "GOOS=darwin go build ./..."
if ! CGO_ENABLED=0 GOOS=darwin go build ./...; then
	echo "darwin build failed"
	fail=1
fi

if [[ "$fail" -ne 0 ]]; then
	echo "platform-check failed"
	exit 1
fi
echo "platform-check ok"
