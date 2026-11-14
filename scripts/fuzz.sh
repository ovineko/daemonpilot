#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
fuzztime=${1:-60s}
export GOMAXPROCS="${GOMAXPROCS:-2}"
status=0
packages=$(go list ./...)
for package in $packages; do
  listed=$(go test -list '^Fuzz' "$package")
  targets=$(printf '%s\n' "$listed" | sed -n '/^Fuzz/p')
  for target in $targets; do
    echo "Fuzzing $package/$target for $fuzztime"
    go test "$package" -run '^$' -fuzz "^$target$" -fuzztime "$fuzztime" || status=1
  done
done
exit "$status"
