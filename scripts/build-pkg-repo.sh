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
SHA256=$(sha256sum "$BINARY" | awk '{print $1}')

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
  "arch": "${ABI}",
  "prefix": "${PREFIX}",
  "flatsize": ${FLATSIZE},
  "categories": ["local"],
  "files": {
    "${PREFIX}/bin/homescreen": "${SHA256}",
    "${PREFIX}/etc/rc.d/homescreen": "-"
  }
}
EOF
)

# +COMPACT_MANIFEST is the same as +MANIFEST for simple packages
echo "$MANIFEST" > "$PKGDIR/+COMPACT_MANIFEST"
echo "$MANIFEST" > "$PKGDIR/+MANIFEST"

# --- Build .pkg (tar.zst with manifests first) ---
mkdir -p repo
PKG_FILENAME="${NAME}-${VERSION}.pkg"

# pkg expects manifests at the start of the archive
(cd "$PKGDIR" && tar cf - +COMPACT_MANIFEST +MANIFEST usr) | zstd -o "repo/${PKG_FILENAME}"

PKGSIZE=$(stat --printf='%s' "repo/${PKG_FILENAME}")
PKGSHA256=$(sha256sum "repo/${PKG_FILENAME}" | awk '{print $1}')

echo "Built repo/${PKG_FILENAME} (${PKGSIZE} bytes)"

# --- Build repository catalog ---
# packagesite.yaml: one JSON object per line
PACKAGESITE=$(cat <<EOF
{"name":"${NAME}","version":"${VERSION}","origin":"local/${NAME}","comment":"Smart home control panel","desc":"MQTT-backed smart home control panel with web UI and SSE","maintainer":"homescreen@local","www":"https://github.com","abi":"${ABI}","arch":"${ABI}","prefix":"${PREFIX}","flatsize":${FLATSIZE},"pkgsize":${PKGSIZE},"sum":"${PKGSHA256}","path":"${PKG_FILENAME}","repopath":"${PKG_FILENAME}","categories":["local"]}
EOF
)

# Create catalog content and sign it.
# IMPORTANT: pkg expects the tar entry to be named "data" (not "packagesite.yaml")
# because repo->meta->data defaults to "data" in pkg's meta handling.
TMPCAT=$(mktemp -d)
echo "$PACKAGESITE" > "$TMPCAT/data"

# Sign the catalog to match FreeBSD pkg's verification protocol.
# pkg verifies via ossl_verify_cert_cb which does:
#   1. hex_sha256 = SHA256_HEX(content)
#   2. hash = SHA256_RAW(hex_sha256)   (raw 32-byte hash of the hex string)
#   3. EVP_PKEY_verify(ctx, sig, siglen, hash, 32)  with md=SHA256
# So the signature must be PKCS1v15(SHA256_DigestInfo || SHA256(SHA256_HEX(content))).
HEX_SHA256=$(sha256sum "$TMPCAT/data" | awk '{print $1}')
echo -n "$HEX_SHA256" | openssl dgst -sha256 -binary > "$TMPCAT/hash"
openssl pkeyutl -sign -inkey "$SIGNING_KEY" -in "$TMPCAT/hash" \
  -pkeyopt digest:sha256 -out "$TMPCAT/signature"
rm "$TMPCAT/hash"
echo "Signed data catalog with $SIGNING_KEY"

(cd "$TMPCAT" && tar cf - data signature) | zstd -o repo/packagesite.pkg

# data.pkg is a copy used by some pkg versions
cp repo/packagesite.pkg repo/data.pkg

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
