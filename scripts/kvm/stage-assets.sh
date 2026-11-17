#!/usr/bin/env bash
# Stage pinned binaries and workloads under $KVMEXP_STAGE on the benchmark host.
# Inputs (already on the host from earlier experiments):
#   $1  judger-bench linux/amd64 binary built from this branch
#   /tmp/iris-cat-20261101/libjudger.so          stock alpha.4 Judger
#   /tmp/iris-rdt-20261031/workload              cpp-runtime-v1 binary
#   /tmp/iris-cat-20261101/{input,output}.txt    cpp-runtime-v1 I/O
#   ~/iris-variance-exp/wl                       memory workloads (wl.c)
set -euo pipefail
stage=${KVMEXP_STAGE:-/var/tmp/iris-kvm}
bench=${1:?judger-bench binary}
judger_sha=2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103
compute_sha=71100d7247185c5324c4f1f88ea4a149330cb11ca892dad422936053754d7ba3
mkdir -p "$stage"/{assets,workloads,scripts,images,runs}
check() { [[ $(sha256sum "$1" | cut -d' ' -f1) == "$2" ]] || { echo "digest mismatch: $1" >&2; exit 1; }; }

check /tmp/iris-cat-20261101/libjudger.so "$judger_sha"
check /tmp/iris-rdt-20261031/workload "$compute_sha"
install -m 0755 /tmp/iris-cat-20261101/libjudger.so "$stage/assets/libjudger.so"
install -m 0755 "$bench" "$stage/assets/judger-bench"

w=$stage/workloads
rm -rf "$w"
mkdir -p "$w"/{compute,chase6,chase44,stream44}
install -m 0755 /tmp/iris-rdt-20261031/workload "$w/compute/bin"
cp /tmp/iris-cat-20261101/input.txt "$w/compute/input.txt"
cp /tmp/iris-cat-20261101/output.txt "$w/compute/output.txt"
# name, wl arguments, stdin (fixed amount of work; see iris-variance-exp/LABELS.md)
while read -r name a1 a2 input; do
  install -m 0755 "$HOME/iris-variance-exp/wl" "$w/$name/bin"
  printf '%s\n%s\n' "$a1" "$a2" > "$w/$name/args"
  printf '%s\n' "$input" > "$w/$name/input.txt"
  "$w/$name/bin" "$a1" "$a2" < "$w/$name/input.txt" > "$w/$name/output.txt"
done <<'EOF'
chase6 chase 6 20000000
chase44 chase 44 4000000
stream44 stream 44 85
EOF
(cd "$stage" && find assets workloads -type f ! -name SHA256SUMS ! -name '*.tar.gz' -exec sha256sum {} + | sort -k2 > assets/SHA256SUMS)
cat "$stage/assets/SHA256SUMS"
