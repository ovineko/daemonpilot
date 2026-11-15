#!/bin/sh
set -eu

usage() {
  echo 'Usage: scripts/bsdvm.sh pack OUTPUT.tar.gz' >&2
  exit 2
}
[ "$#" -eq 2 ] && [ "$1" = pack ] || usage
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
bundle=$(mktemp -d "${TMPDIR:-/tmp}/daemonpilot-bsdvm.XXXXXX")
trap 'rm -rf "$bundle"' 0
trap 'exit 1' HUP INT TERM
cd "$root"
go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... > "$bundle/packages"
while IFS= read -r package; do
  relative=${package#github.com/ovineko/daemonpilot/}
  mkdir -p "$bundle/$relative"
  if [ -d "$relative/testdata" ]; then cp -R "$relative/testdata" "$bundle/$relative/"; fi
done < "$bundle/packages"
for os in freebsd openbsd netbsd dragonfly; do
  case "$os" in
    freebsd | openbsd | netbsd) arches='amd64 arm64' ;;
    dragonfly) arches='amd64' ;;
  esac
  for arch in $arches; do
    echo "Building $os/$arch with $(go version)"
    export CGO_ENABLED=0 GOOS="$os" GOARCH="$arch"
    mkdir -p "$bundle/$os/$arch/fixtures"
    go build -trimpath -o "$bundle/$os/$arch/fixtures/lifecycle" ./test/fixtures/lifecycle
    go build -trimpath -o "$bundle/$os/$arch/fixtures/process" ./test/integration/testdata/processfixture
    go build -trimpath -o "$bundle/$os/$arch/fixtures/update" ./internal/watchdog/testdata/updatefixture
    while IFS= read -r package; do
      relative=${package#github.com/ovineko/daemonpilot/}
      mkdir -p "$bundle/$os/$arch/$relative"
      go test -c -trimpath -o "$bundle/$os/$arch/$relative/suite.test" "$package"
    done < "$bundle/packages"
  done
done
cat > "$bundle/run.sh" << 'RUNNER'
#!/bin/sh
set -eu
root=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in dragonfly) ;; freebsd|openbsd|netbsd) ;; *) echo "Unsupported guest OS: $os" >&2; exit 2 ;; esac
case $(uname -m) in
  amd64|x86_64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "Unsupported guest architecture: $(uname -m)" >&2; exit 2 ;;
esac
bundle="$root/$os/$arch"
[ -d "$bundle" ] || { echo "No cross-built bundle for $os/$arch" >&2; exit 2; }
printf 'Guest: '; uname -a
printf 'Test binary target: %s/%s\n' "$os" "$arch"
printf 'Cross-build Go toolchain: %s\n' "$(cat "$root/go-version")"
case "$arch" in
  arm64) export DAEMONPILOT_TEST_TIME_SCALE=${DAEMONPILOT_TEST_TIME_SCALE:-8} ;;
  *) export DAEMONPILOT_TEST_TIME_SCALE=${DAEMONPILOT_TEST_TIME_SCALE:-1} ;;
esac
export DAEMONPILOT_TEST_FIXTURES="$bundle/fixtures"
cd "$root"
status=0
while IFS= read -r package; do
  relative=${package#github.com/ovineko/daemonpilot/}
  echo "Running $package"
  (cd "$root/$relative" && "$bundle/$relative/suite.test" -test.v -test.timeout=30m) || status=1
done < "$root/packages"
if [ "$status" -eq 0 ]; then echo "$os/$arch PASS"; else echo "$os/$arch FAIL"; fi
exit "$status"
RUNNER
printf '%s\n' "$(go version)" > "$bundle/go-version"
chmod +x "$bundle/run.sh"
tar -czf "$2" -C "$bundle" .
