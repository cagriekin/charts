#!/bin/bash
# Static checks on the pg-extensions Dockerfile (#320). No build, no cluster -- these are
# the invariants that make the image safe to copy from, and each one has a failure mode
# that is silent at build time and only shows up as an extension-less or ABI-mismatched
# cluster later.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DF="${ROOT}/Dockerfile"
fail=0
ok()  { echo "PASS: $1"; }
bad() { echo "FAIL: $1"; fail=1; }

[ -f "${DF}" ] || { echo "FAIL: no Dockerfile at ${DF}"; exit 1; }

# --- the base is digest-pinned, like images/repmgr/Dockerfile ---
# A floating debian:trixie-slim would let the C runtime under the extension .so files move
# without anything in this repo changing -- the exact ABI drift #302 is about, except here
# it would arrive via a rebuild rather than a chart change.
if grep -qE '^FROM debian:trixie-slim@sha256:[0-9a-f]{64}$' "${DF}"; then
  ok "#320: base image is digest-pinned"
else
  bad "#320: base image is not digest-pinned (FROM debian:trixie-slim@sha256:...)"
fi

# --- ARG PG_MAJOR default matches images/repmgr/Dockerfile ---
# The two images are copied into the SAME ext-lib/ext-share volumes. If their majors can
# differ by default, a build that omits --build-arg produces extensions for a major the
# server never runs, and PostgreSQL rejects the .so at CREATE EXTENSION time -- long after
# the build looked fine.
ext_major=$(grep -oE '^ARG PG_MAJOR=[0-9]+' "${DF}" | head -1 | cut -d= -f2)
repmgr_major=$(grep -oE '^ARG PG_MAJOR=[0-9]+' "${ROOT}/../repmgr/Dockerfile" | head -1 | cut -d= -f2)
if [ -n "${ext_major}" ] && [ "${ext_major}" = "${repmgr_major}" ]; then
  ok "#320: ARG PG_MAJOR default (${ext_major}) matches the repmgr image"
else
  bad "#320: ARG PG_MAJOR default '${ext_major}' does not match the repmgr image's '${repmgr_major}'"
fi

# --- PACKAGES is required at build time ---
# An image with no extensions is never what anyone wanted, and it is indistinguishable from
# a working one until every pod comes up missing the extension it was supposed to gain.
if grep -q 'if \[ -z "\$PACKAGES" \]' "${DF}" && grep -q 'PACKAGES is required' "${DF}"; then
  ok "#320: build fails when PACKAGES is empty"
else
  bad "#320: build does not require PACKAGES"
fi

# --- the build verifies BOTH trees the chart copies from are populated ---
# A package name that exists but installs nothing for this major (a metapackage, a docs-only
# package, the wrong major in the name) would otherwise build cleanly and produce an image the
# chart copies nothing out of. Both directories matter, not just the extension one: the chart's
# copy command reads /usr/lib/postgresql/<major>/lib FIRST and &&-chained, so an empty lib dir
# crash-loops copy-prebuilt-ext with `cp: cannot stat`.
missing_dir_check=0
for d in '/usr/share/postgresql/$PG_MAJOR/extension' '/usr/lib/postgresql/$PG_MAJOR/lib'; do
  grep -qF "${d}" "${DF}" || missing_dir_check=1
done
if [ "${missing_dir_check}" -eq 0 ] && grep -q 'ls -A' "${DF}"; then
  ok "#320: build verifies both copied directories are non-empty"
else
  bad "#320: build does not verify both /usr/share/postgresql/<major>/extension and /usr/lib/postgresql/<major>/lib"
fi

# --- each step reports its own failure ---
# An `&&` chain with one trailing `||` blamed an apt/proxy/pin failure on "check PACKAGES
# names", sending the operator to look at the wrong thing.
if grep -q 'RUN set -eu' "${DF}"; then
  ok "#320: the install step uses set -eu so each failure reports itself"
else
  bad "#320: the install step does not use set -eu (one trailing || would mask apt failures)"
fi

# --- an optional apt source must be signed ---
# The chart refuses trusted=/allow-insecure= in aptSources for the same reason
# (pg.validateExtensionAptSources): without signed-by, fetching and dearmoring the key is
# decorative and the build installs unsigned packages as root.
if grep -q 'signed-by=' "${DF}" && grep -q 'APT_SOURCE_LINE must carry signed-by' "${DF}"; then
  ok "#320: an APT_SOURCE_LINE without signed-by fails the build"
else
  bad "#320: APT_SOURCE_LINE is not required to be signed"
fi

# --- the three APT_SOURCE_* args are all-or-nothing ---
# Two of three set silently produces an image missing the source it was meant to add, and
# the missing extension only surfaces at CREATE EXTENSION.
if grep -q 'must be set together' "${DF}"; then
  ok "#320: partial APT_SOURCE_* configuration fails the build"
else
  bad "#320: partial APT_SOURCE_* configuration is not rejected"
fi

# --- no ENTRYPOINT/CMD ---
# Nothing runs this image as a workload; the chart overrides the command with a plain cp.
# An ENTRYPOINT here would be dead weight that invites someone to run it as a server.
if grep -qE '^\s*(ENTRYPOINT|CMD)\b' "${DF}"; then
  bad "#320: Dockerfile declares an ENTRYPOINT/CMD (nothing ever runs this image)"
else
  ok "#320: no ENTRYPOINT/CMD (the chart supplies the copy command)"
fi

# --- apt lists are cleaned up ---
# 11 MB of package indexes in an image whose entire job is to be copied out of.
if [ "$(grep -c 'rm -rf /var/lib/apt/lists' "${DF}")" -ge 2 ]; then
  ok "#320: apt lists are removed after each install"
else
  bad "#320: apt lists are not cleaned up in every install layer"
fi

# --- the chart's copy paths match what the image populates ---
# The contract between this Dockerfile and pg.extensionPrebuiltCopyCommand. If they drift,
# the copy silently produces an empty ext-share and every pod comes up without extensions.
# Scoped to the pg.extensionPrebuiltCopyCommand define, and matching the FULL cp literal.
# An unscoped substring grep passed on pg.extensionInstallCommand's own copy lines, which
# carry the same paths -- so a change to the prebuilt copy command alone, the exact drift the
# workflow's pg/templates/_helpers.tpl paths entry was added to catch, went undetected.
HELPERS="${ROOT}/../../pg/templates/_helpers.tpl"
PREBUILT=$(sed -n '/define "pg.extensionPrebuiltCopyCommand"/,/^{{- end }}/p' "${HELPERS}")
if [ -z "${PREBUILT}" ]; then
  bad "#320: pg.extensionPrebuiltCopyCommand not found in _helpers.tpl (renamed or removed)"
else
  for lit in 'cp -n /usr/lib/postgresql/%s/lib/*.so* /ext-lib/' 'cp -n /usr/share/postgresql/%s/extension/* /ext-share/'; do
    if printf '%s\n' "${PREBUILT}" | grep -qF "${lit}"; then
      ok "#320: prebuilt copy command still uses: ${lit}"
    else
      bad "#320: prebuilt copy command no longer matches this Dockerfile: expected ${lit}"
    fi
  done
fi

echo "----"
if [ "${fail}" -eq 0 ]; then echo "ALL TESTS PASSED"; else echo "SOME TESTS FAILED"; fi
exit "${fail}"
