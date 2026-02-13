#!/usr/bin/env bash
#
# Build a FreeBSD .pkg file and a static pkg repository.
# Usage: build-pkg-repo.sh <version>
#
# Produces a repo/ directory ready to serve as a FreeBSD pkg repo.
#
set -euo pipefail

VERSION="${1:?Usage: build-pkg-repo.sh <version> <signing-key>}"
SIGNING_KEY="${2:?Usage: build-pkg-repo.sh <version> <signing-key>}"
NAME="homescreen"
ABI="FreeBSD:13:amd64"
PREFIX="/usr/local"
BINARY="homescreen"   # must already be built for FreeBSD

# --- Sanity check (warning only; 'file' output varies) ---
if ! file "$BINARY" | grep -qi 'freebsd\|x86-64'; then
  echo "WARNING: $BINARY may not be a FreeBSD/amd64 binary" >&2
fi

FLATSIZE=$(stat --printf='%s' "$BINARY")

# --- Stage package contents ---
# Use absolute paths to match FreeBSD pkg convention.
PKGDIR=$(mktemp -d)
trap 'rm -rf "$PKGDIR"' EXIT

mkdir -p "$PKGDIR/usr/local/bin"
mkdir -p "$PKGDIR/usr/local/etc/rc.d"
cp "$BINARY" "$PKGDIR/usr/local/bin/homescreen"
chmod 755 "$PKGDIR/usr/local/bin/homescreen"

# --- Create rc.d script ---
cat > "$PKGDIR/usr/local/etc/rc.d/homescreen" << 'RCEOF'
#!/bin/sh

# PROVIDE: homescreen
# REQUIRE: DAEMON mosquitto
# KEYWORD: shutdown

. /etc/rc.subr

name="homescreen"
rcvar="homescreen_enable"

load_rc_config $name

: ${homescreen_enable:="NO"}
: ${homescreen_user:="root"}

pidfile="/var/run/${name}.pid"
command="/usr/local/bin/homescreen"
command_args="& echo \$! > ${pidfile}"

start_cmd="homescreen_start"
stop_cmd="homescreen_stop"
status_cmd="homescreen_status"

homescreen_start() {
  echo "Starting ${name}."
  /usr/sbin/daemon -p ${pidfile} -u ${homescreen_user} ${command}
}

homescreen_stop() {
  if [ -f ${pidfile} ]; then
    echo "Stopping ${name}."
    kill $(cat ${pidfile}) 2>/dev/null
    rm -f ${pidfile}
  fi
}

homescreen_status() {
  if [ -f ${pidfile} ] && kill -0 $(cat ${pidfile}) 2>/dev/null; then
    echo "${name} is running as pid $(cat ${pidfile})."
  else
    echo "${name} is not running."
    return 1
  fi
}

run_rc_command "$1"
RCEOF
chmod 755 "$PKGDIR/usr/local/etc/rc.d/homescreen"

# --- Build manifests ---
BIN_SHA256=$(sha256sum "$PKGDIR/usr/local/bin/homescreen" | awk '{print $1}')
RCD_SHA256=$(sha256sum "$PKGDIR/usr/local/etc/rc.d/homescreen" | awk '{print $1}')

# +COMPACT_MANIFEST has no files detail
COMPACT_MANIFEST=$(cat <<EOF
{
  "name": "${NAME}",
  "version": "${VERSION}",
  "origin": "local/${NAME}",
  "comment": "Smart home control panel",
  "desc": "MQTT-backed smart home control panel with web UI and SSE",
  "maintainer": "homescreen@local",
  "www": "https://github.com",
  "abi": "${ABI}",
  "arch": "freebsd:13:x86:64",
  "prefix": "${PREFIX}",
  "flatsize": ${FLATSIZE},
  "categories": ["local"]
}
EOF
)

# +MANIFEST includes file entries with checksums (matching real FreeBSD pkg format)
MANIFEST=$(cat <<EOF
{
  "name": "${NAME}",
  "version": "${VERSION}",
  "origin": "local/${NAME}",
  "comment": "Smart home control panel",
  "desc": "MQTT-backed smart home control panel with web UI and SSE",
  "maintainer": "homescreen@local",
  "www": "https://github.com",
  "abi": "${ABI}",
  "arch": "freebsd:13:x86:64",
  "prefix": "${PREFIX}",
  "flatsize": ${FLATSIZE},
  "categories": ["local"],
  "files": {
    "${PREFIX}/bin/homescreen": {
      "sum": "1\$${BIN_SHA256}",
      "uname": "root",
      "gname": "wheel",
      "perm": "0555"
    },
    "${PREFIX}/etc/rc.d/homescreen": {
      "sum": "1\$${RCD_SHA256}",
      "uname": "root",
      "gname": "wheel",
      "perm": "0555"
    }
  }
}
EOF
)

echo "$COMPACT_MANIFEST" > "$PKGDIR/+COMPACT_MANIFEST"
echo "$MANIFEST" > "$PKGDIR/+MANIFEST"

# --- Build .pkg (tar.zst with manifests first, absolute paths, no directory entries) ---
mkdir -p repo
PKG_FILENAME="${NAME}-${VERSION}.pkg"

# pkg expects: manifests first (relative), then files with absolute paths, no directory entries.
# We use transform to prepend / to file paths while keeping manifests relative.
tar cf - \
  --owner=root --group=wheel \
  -C "$PKGDIR" \
  +COMPACT_MANIFEST \
  +MANIFEST \
  --transform='s|^usr|/usr|' \
  --no-recursion \
  usr/local/bin/homescreen \
  usr/local/etc/rc.d/homescreen \
  | zstd -o "repo/${PKG_FILENAME}"

PKGSIZE=$(stat --printf='%s' "repo/${PKG_FILENAME}")
PKGSHA256=$(sha256sum "repo/${PKG_FILENAME}" | awk '{print $1}')

echo "Built repo/${PKG_FILENAME} (${PKGSIZE} bytes)"

# --- Build repository catalogs ---
# Package entry shared by both formats
PKG_ENTRY=$(cat <<EOF
{"name":"${NAME}","version":"${VERSION}","origin":"local/${NAME}","comment":"Smart home control panel","desc":"MQTT-backed smart home control panel with web UI and SSE","maintainer":"homescreen@local","www":"https://github.com","abi":"${ABI}","arch":"${ABI}","prefix":"${PREFIX}","flatsize":${FLATSIZE},"pkgsize":${PKGSIZE},"sum":"${PKGSHA256}","path":"${PKG_FILENAME}","repopath":"${PKG_FILENAME}","categories":["local"]}
EOF
)

sign_catalog() {
  local content_file="$1"
  local out_sig="$2"
  # FreeBSD pkg verification protocol (ossl_verify_cert_cb):
  #   1. hex_sha256 = SHA256_HEX(content)
  #   2. hash = SHA256_RAW(hex_sha256)
  #   3. EVP_PKEY_verify(ctx, sig, hash, 32) with md=SHA256
  local hex
  hex=$(sha256sum "$content_file" | awk '{print $1}')
  echo -n "$hex" | openssl dgst -sha256 -binary > "${content_file}.hash"
  openssl pkeyutl -sign -inkey "$SIGNING_KEY" -in "${content_file}.hash" \
    -pkeyopt digest:sha256 -out "$out_sig"
  rm "${content_file}.hash"
}

TMPCAT=$(mktemp -d)

# data.pkg — pkg 2.x format: JSON object with "packages" array.
# Entry name inside tar must be "data" (repo->meta->data default).
cat > "$TMPCAT/data" <<EOF
{"packages":[${PKG_ENTRY}]}
EOF
sign_catalog "$TMPCAT/data" "$TMPCAT/signature"
(cd "$TMPCAT" && tar cf - data signature) | zstd -o repo/data.pkg
rm "$TMPCAT/data" "$TMPCAT/signature"
echo "Built and signed data.pkg (v2 format)"

# packagesite.pkg — legacy format: one JSON object per line.
# Entry name inside tar must be "packagesite.yaml" (repo->meta->manifests default).
echo "$PKG_ENTRY" > "$TMPCAT/packagesite.yaml"
sign_catalog "$TMPCAT/packagesite.yaml" "$TMPCAT/signature"
(cd "$TMPCAT" && tar cf - packagesite.yaml signature) | zstd -o repo/packagesite.pkg
rm "$TMPCAT/packagesite.yaml" "$TMPCAT/signature"
echo "Built and signed packagesite.pkg (legacy format)"

# meta.conf
cat > repo/meta.conf << 'METAEOF'
version = 2;
packing_format = "tzst";
METAEOF

# Also create meta.pkg (tar.zst of meta.conf)
(cd repo && tar cf - meta.conf) | zstd -o repo/meta.pkg

rm -rf "$TMPCAT"

echo ""
echo "Repository built in repo/:"
ls -lh repo/
echo ""
echo "Configure on FreeBSD with:"
echo '  mkdir -p /usr/local/etc/pkg/repos /usr/local/etc/pkg/keys'
echo '  # Copy homescreen-repo.pub to /usr/local/etc/pkg/keys/homescreen.pub'
echo '  cat > /usr/local/etc/pkg/repos/homescreen.conf << EOF'
echo '  homescreen: {'
echo '    url: "https://<user>.github.io/<repo>"'
echo '    enabled: yes'
echo '    signature_type: "pubkey"'
echo '    pubkey: "/usr/local/etc/pkg/keys/homescreen.pub"'
echo '  }'
echo '  EOF'
