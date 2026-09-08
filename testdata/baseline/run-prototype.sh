# Reference run of the Bash prototype, used as the behavioural baseline the Go
# implementation must match or consciously improve on.
#   docker run --rm -v /path/to/workspace:/work debian:bookworm-slim bash /work/debark/testdata/baseline/run-prototype.sh
set -e
out=/work/debark/testdata/baseline
cd /tmp && mkdir -p base && cd base
printf 'jq\ntree\n' > pkgs.txt
/work/download-packages.sh --state "$out/../real-targets/debian-12-state.tar.gz" \
    -f pkgs.txt --out /tmp/base/bundle >/tmp/base/run.log 2>&1 || { tail -20 /tmp/base/run.log; exit 1; }
mkdir -p "$out/bundle-metadata"
cp /tmp/base/bundle/debs/Packages "$out/bundle-metadata/Packages"
cp /tmp/base/bundle/bundle-info.txt "$out/bundle-metadata/bundle-info.txt" 2>/dev/null || true
cp /tmp/base/bundle/manifest.txt "$out/bundle-metadata/manifest.txt" 2>/dev/null || true
( cd /tmp/base/bundle && find . -type f | sort ) > "$out/bundle-metadata/tree.txt"
command -v apt-ftparchive >/dev/null && echo "apt-ftparchive: present" > "$out/bundle-metadata/env.txt" \
    || echo "apt-ftparchive: ABSENT (no Release file generated)" > "$out/bundle-metadata/env.txt"
apt-get -v | head -1 >> "$out/bundle-metadata/env.txt"
echo done
