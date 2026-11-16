#!/bin/sh
set -eu

usage() {
  echo 'Usage: scripts/crossarch.sh [docker | pack OUTPUT.tar.gz]' >&2
  exit 2
}
mode=${1:-docker}
case "$mode" in
  docker) [ "$#" -le 1 ] || usage ;;
  pack) [ "$#" -eq 2 ] || usage ;;
  *) usage ;;
esac
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
bundle=$(mktemp -d "${TMPDIR:-/tmp}/daemonpilot-crossarch.XXXXXX")
trap 'rm -rf "$bundle"' 0
trap 'exit 1' HUP INT TERM
cd "$root"
go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... > "$bundle/packages"
while IFS= read -r package; do
  relative=${package#github.com/ovineko/daemonpilot/}
  mkdir -p "$bundle/$relative"
  if [ -d "$relative/testdata" ]; then cp -R "$relative/testdata" "$bundle/$relative/"; fi
done < "$bundle/packages"
for arch in amd64 arm64 riscv64 arm 386; do
  echo "Building linux/$arch"
  export CGO_ENABLED=0 GOOS=linux GOARCH=$arch GOARM=7
  mkdir -p "$bundle/$arch/fixtures"
  go build -trimpath -o "$bundle/$arch/fixtures/lifecycle" ./test/fixtures/lifecycle
  go build -trimpath -o "$bundle/$arch/fixtures/process" ./test/integration/testdata/processfixture
  go build -trimpath -o "$bundle/$arch/fixtures/update" ./internal/watchdog/testdata/updatefixture
  go build -trimpath -o "$bundle/$arch/fixtures/probe" ./internal/updater/testdata/probe
  for version in v2 v3; do
    go build -trimpath -ldflags "-X main.version=$version" -o "$bundle/$arch/fixtures/execfixture-$version" ./test/integration/testdata/execfixture
  done
  for fixture_mode in run exit0 exit1 exit2 count-exit1 marker-exit1; do
    go build -trimpath -ldflags "-X main.mode=$fixture_mode" -o "$bundle/$arch/fixtures/watchdog-worker-$fixture_mode" ./internal/watchdog/testdata/portable_worker.go
  done
  for version in v1 v2 vreg; do
    go build -trimpath -ldflags "-X main.version=$version" -o "$bundle/$arch/fixtures/crashfixture-$version" ./internal/updater/testdata/crashfixture
  done
  while IFS= read -r package; do
    relative=${package#github.com/ovineko/daemonpilot/}
    mkdir -p "$bundle/$arch/$relative"
    go test -c -trimpath -o "$bundle/$arch/$relative/suite.test" "$package"
  done < "$bundle/packages"
done
cat > "$bundle/run.sh" << 'RUNNER'
#!/bin/sh
set -eu
root=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
arch=${1:-}
if [ -z "$arch" ]; then
    case $(uname -m) in
        x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;;
        riscv64) arch=riscv64 ;; armv7*|armv8l) arch=arm ;; i?86) arch=386 ;;
        *) echo 'Pass the bundle architecture: amd64, arm64, riscv64, arm, 386' >&2; exit 2 ;;
    esac
fi
case "$arch" in amd64|arm64|riscv64|arm|386) ;; *) exit 2 ;; esac
export DAEMONPILOT_TEST_TIME_SCALE=${DAEMONPILOT_TEST_TIME_SCALE:-1}
export DAEMONPILOT_TEST_FIXTURES="$root/$arch/fixtures"
cd "$root"
status=0
while IFS= read -r package; do
  relative=${package#github.com/ovineko/daemonpilot/}
  (cd "$root/$relative" && "$root/$arch/$relative/suite.test" -test.v -test.timeout=30m) || status=1
done < "$root/packages"
if [ "$status" -eq 0 ]; then echo "linux/$arch PASS"; else echo "linux/$arch FAIL"; fi
exit "$status"
RUNNER
chmod +x "$bundle/run.sh"
if [ "$mode" = pack ]; then
  tar -czf "$2" -C "$bundle" .
  echo "Bundle written: $2"
  exit 0
fi
host=$(docker info --format '{{.Architecture}}')
case "$host" in x86_64 | amd64) host=amd64 ;; aarch64 | arm64) host=arm64 ;; armv7*) host=arm ;; esac
# Each architecture runs from its own image digest, never the shared tag:
# `docker run --platform <foreign> <tag>` re-points that local tag at the
# foreign image, and Docker treats 386 as amd64-compatible, so every later
# amd64 user of it silently got a 32-bit userland. The classic image store
# also cannot hold several platforms under one index digest.
base_image_for() {
  case "$1" in
    amd64) echo debian@sha256:7792b1f7702a86946cd518db72b6a407302c3e9bc1635634368b878189e8221c ;;
    arm64) echo debian@sha256:da496358bd6934d2bd6a563a33176a2e50eff5490c54b4ac6fb051b69fef4071 ;;
    riscv64) echo debian@sha256:e3b4cb1b0056896c4b43e17472b906b10007ab94bc533d37a47e9a5795eebbc0 ;;
    arm) echo debian@sha256:36fb0164b892c70d3f7e84d2650f0550276f6099381cce5f2d15de87271709f4 ;;
    386) echo debian@sha256:925b2e26c366e1666a32f47809d3428de3db478ea5ef6bcd73c3d54a415c9285 ;;
  esac
}
status=0
for arch in amd64 arm64 riscv64 arm 386; do
  platform=linux/$arch
  [ "$arch" != arm ] || platform=linux/arm/v7
  scale=8
  emulated=1
  if [ "$arch" = "$host" ] || { [ "$arch" = 386 ] && [ "$host" = amd64 ]; }; then
    scale=1
    emulated=0
  fi
  echo "Running $platform (time scale $scale)"
  if docker run --rm --init --platform "$platform" --network none \
    --mount "type=bind,src=$bundle,dst=/bundle,readonly" \
    --env "DAEMONPILOT_TEST_TIME_SCALE=$scale" \
    --env "DAEMONPILOT_TEST_EMULATED=$emulated" \
    "$(base_image_for "$arch")" /bundle/run.sh "$arch"; then
    echo "$platform PASS"
  else
    echo "$platform FAIL"
    status=1
  fi
done
exit "$status"
