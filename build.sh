#!/usr/bin/env bash
set -euo pipefail

# Builds this plugin as a c-shared dynamic library.
# c-shared requires CGO_ENABLED=1 and a C toolchain (gcc / mingw-w64).
# The SQLite driver itself is pure Go (modernc.org/sqlite).

cd "$(dirname "$0")"
plugin_id="$(sed -n 's/^const PluginID = "\([^"]*\)".*/\1/p' internal/protocol/constants.go)"
if [[ -z "${plugin_id}" ]]; then
  echo "Cannot determine plugin ID" >&2
  exit 1
fi
out_dir="${1:-.}"
mkdir -p "${out_dir}"

ext="so"
case "$(go env GOOS)" in
  windows) ext="dll" ;;
  darwin) ext="dylib" ;;
esac

ldflags="-s -w"
if [[ -n "${PLUGIN_VERSION:-}" ]]; then
  ldflags="${ldflags} -X 'main.pluginVersion=${PLUGIN_VERSION}'"
fi
if [[ -n "${PLUGIN_AUTHOR:-}" ]]; then
  ldflags="${ldflags} -X 'main.pluginAuthor=${PLUGIN_AUTHOR}'"
fi
if [[ -n "${PLUGIN_REPOSITORY:-}" ]]; then
  ldflags="${ldflags} -X 'main.pluginRepository=${PLUGIN_REPOSITORY}'"
fi

artifact="${out_dir}/${plugin_id}.${ext}"
CGO_ENABLED="${CGO_ENABLED:-1}" go build -trimpath -ldflags="${ldflags}" -buildmode=c-shared -o "${artifact}" .
if command -v strip >/dev/null 2>&1; then
  strip "${artifact}" 2>/dev/null || true
fi
echo "Built ${artifact}"
