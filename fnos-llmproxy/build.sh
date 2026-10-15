#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
VERSION="${VERSION:-1.2.1}"
COMMIT="${COMMIT:-$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo local)}"
OUT="$ROOT/dist"
PKG_SRC="$ROOT/packaging/llmproxy"

mkdir -p "$OUT"

md5_file() {
  if command -v md5sum >/dev/null 2>&1; then
    md5sum "$1" | awk '{print $1}'
  else
    md5 -q "$1"
  fi
}

manual_pack() {
  local stage="$1"
  local suffix="$2"
  local platform="$3"
  local work="$OUT/manual-$suffix"
  local pkg="$work/package"
  local app_tgz="$pkg/app.tgz"
  local outfile="$OUT/llmproxy_${VERSION}_${platform}.fpk"

  rm -rf "$work"
  mkdir -p "$pkg/cmd" "$pkg/config" "$pkg/wizard"

  tar -C "$stage/app" -czf "$app_tgz" .
  local checksum
  checksum="$(md5_file "$app_tgz")"

  cp -a "$stage/cmd/." "$pkg/cmd/"
  cp -a "$stage/config/." "$pkg/config/"
  cp -a "$stage/wizard/." "$pkg/wizard/" 2>/dev/null || true
  cp "$stage/ICON.PNG" "$stage/ICON_256.PNG" "$pkg/"
  cp "$stage/LICENSE" "$pkg/" 2>/dev/null || true
  cp "$stage/manifest" "$pkg/manifest"
  sed -i "s/^checksum=.*/checksum=$checksum/" "$pkg/manifest"

  (
    cd "$pkg"
    tar -czf "$outfile" .
  )

  sha256sum "$outfile" > "$outfile.sha256"
  echo "Manual FPK: $outfile"
}

build_arch() {
  local goarch="$1"
  local fnarch="$2"
  local suffix="$3"
  local stage="$OUT/stage-$suffix"

  rm -rf "$stage"
  cp -a "$PKG_SRC" "$stage"
  sed -i "s/^platform=.*/platform=$fnarch/" "$stage/manifest"
  sed -i "s/^version=.*/version=$VERSION/" "$stage/manifest"

  echo "Building linux/$goarch..."
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT" \
    -o "$stage/app/bin/llmproxy" "$ROOT/cmd/llmproxy"

  find "$stage" -type d -exec chmod 0755 {} +
  find "$stage" -type f -exec chmod 0644 {} +
  chmod 0755 "$stage/app/bin/llmproxy" "$stage"/cmd/*

  cp "$stage/app/bin/llmproxy" "$OUT/llmproxy-linux-$suffix"
  chmod 0755 "$OUT/llmproxy-linux-$suffix"

  manual_pack "$stage" "$suffix" "$fnarch"

  if command -v fnpack >/dev/null 2>&1; then
    echo "Packaging $suffix with official fnpack..."
    local fnstage="$OUT/fnpack-stage-$suffix"
    rm -rf "$fnstage"
    cp -a "$stage" "$fnstage"
    (
      cd "$OUT"
      fnpack build --directory "$fnstage"
    )
    if [ -f "$OUT/llmproxy.fpk" ]; then
      mv "$OUT/llmproxy.fpk" "$OUT/llmproxy_${VERSION}_${fnarch}_fnpack.fpk"
      sha256sum "$OUT/llmproxy_${VERSION}_${fnarch}_fnpack.fpk" > "$OUT/llmproxy_${VERSION}_${fnarch}_fnpack.fpk.sha256"
    fi
  fi
}

rm -f "$OUT"/*.fpk "$OUT"/*.fpk.sha256 "$OUT"/llmproxy-linux-* 2>/dev/null || true
build_arch amd64 x86 amd64
build_arch arm64 arm arm64

echo "Done. Artifacts: $OUT"
