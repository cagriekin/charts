# Prebuilt PostgreSQL extension image for the pg/pgvector charts (#320).
#
# WHY THIS EXISTS
#
# With postgresql.extensions.packages set, the chart's copy-base-ext and copy-ext init
# containers run `apt-get update` + `apt-get install` on EVERY pod (re)start -- ext-lib,
# ext-share and ext-extra-lib are emptyDirs, so nothing is cached between restarts. That is
# twice per pod, times every replica, on every crash, eviction, rolling update and scale-up.
#
# Under a per-namespace default-deny egress policy the repeated work is not the real cost --
# the HOSTS are. Every external host the install touches has to sit in the platform's
# baseline allow, for every tenant, permanently: apt.postgresql.org, repo.pigsty.io, and
# deb.debian.org (the general-purpose Debian archive, pulled in for a single transitive
# dependency like libsodium23, which neither PGDG nor Pigsty ships).
#
# This image resolves the packages ONCE, at build time. Point the chart at it with
# postgresql.extensions.image and the pods do a plain `cp` from its filesystem: no apt at pod
# start, no egress at pod start, and -- because it is only a copy -- no root and no
# PSA-restricted exemption either, unlike the apt path.
#
# WHY IT IS PARAMETERIZED RATHER THAN PUBLISHED
#
# The useful package set is per-deployment (a Supabase-shaped set looks nothing like a
# PostGIS one), so there is no canonical tag worth publishing. This is a build recipe: build
# it with your own list, push it to your own registry, and pin the tag in values. Building it
# once in your own CI is the point -- that is where the egress belongs.
#
# BUILD
#
#   docker build -t registry.internal/pg-extensions:18-v1 \
#     --build-arg PG_MAJOR=18 \
#     --build-arg PACKAGES="postgresql-18-cron postgresql-18-pgvector" \
#     images/pg-extensions
#
# With a non-PGDG source (Pigsty packages pgsodium, supabase_vault, pg_graphql, pg_net,
# supautils, wrappers, pgjwt, pgmq -- none of which PGDG ships):
#
#   docker build -t registry.internal/pg-extensions:18-supabase \
#     --build-arg PG_MAJOR=18 \
#     --build-arg PACKAGES="postgresql-18-pgsodium postgresql-18-vault" \
#     --build-arg APT_SOURCE_NAME=pigsty \
#     --build-arg APT_SOURCE_KEY_URL=https://repo.pigsty.io/key \
#     --build-arg APT_SOURCE_LINE="deb [signed-by=/usr/share/keyrings/pigsty-keyring.gpg] https://repo.pigsty.io/apt/pgsql/trixie trixie main" \
#     images/pg-extensions
#
# Then:
#
#   postgresql:
#     majorVersion: "18"
#     extensions:
#       enabled: true
#       image:
#         repository: registry.internal/pg-extensions
#         tag: "{major}-supabase"        # {major} substitutes postgresql.majorVersion
#       extraLibs:
#         - /usr/lib/x86_64-linux-gnu/libsodium.so.23
#
# packages/aptSources must be EMPTY when image is set -- the chart refuses both at render
# time, because two sources for the same extension is exactly the "which one wins" question
# nobody should have to answer at 3am.

# Digest-pinned (manifest-list digest, multi-arch), matching images/repmgr/Dockerfile.
# Refresh with: docker buildx imagetools inspect debian:trixie-slim
FROM debian:trixie-slim@sha256:4e401d95de7083948053197a9c3913343cd06b706bf15eb6a0c3ccd26f436a0e

# PostgreSQL major the extensions are built for. Must match the chart's
# postgresql.majorVersion: an extension .so is built against one major's server ABI and
# will not load into another. Default 18, matching images/repmgr/Dockerfile's ARG PG_MAJOR
# so the two images' defaults cannot drift apart silently.
ARG PG_MAJOR=18

# Space-separated Debian package names, e.g. "postgresql-18-cron postgresql-18-pgvector".
# Version pins work the same as in the chart's packages list ("postgresql-18-cron=1.6.4-1").
ARG PACKAGES=""

# Optional non-PGDG apt source, mirroring the chart's extensions.aptSources triple. All
# three must be set together or none at all.
ARG APT_SOURCE_NAME=""
ARG APT_SOURCE_KEY_URL=""
ARG APT_SOURCE_LINE=""

# `apt-get upgrade` first, for the same reason images/repmgr/Dockerfile does it: the
# digest-pinned base is a snapshot, and without this the image ships whatever Debian
# security updates landed after it as stale pre-installed versions.
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    curl \
    gnupg \
    lsb-release \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# PGDG, configured exactly as images/repmgr/Dockerfile does -- the extension packages this
# image exists to install come from here.
RUN curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
      | gpg --dearmor -o /usr/share/keyrings/postgresql-keyring.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/postgresql-keyring.gpg] http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
      > /etc/apt/sources.list.d/postgresql.list

# The optional extra source, keyed under its own name so it can never collide with the PGDG
# keyring above. signed-by is required in APT_SOURCE_LINE (the build fails below without it):
# an unsigned source would make this whole key-verification step decorative.
RUN if [ -n "$APT_SOURCE_NAME" ] || [ -n "$APT_SOURCE_KEY_URL" ] || [ -n "$APT_SOURCE_LINE" ]; then \
      if [ -z "$APT_SOURCE_NAME" ] || [ -z "$APT_SOURCE_KEY_URL" ] || [ -z "$APT_SOURCE_LINE" ]; then \
        echo "APT_SOURCE_NAME, APT_SOURCE_KEY_URL and APT_SOURCE_LINE must be set together" >&2; exit 1; \
      fi; \
      case "$APT_SOURCE_LINE" in \
        *signed-by=*) : ;; \
        *) echo "APT_SOURCE_LINE must carry signed-by=/usr/share/keyrings/${APT_SOURCE_NAME}-keyring.gpg -- an unsigned source makes the key fetch decorative" >&2; exit 1 ;; \
      esac; \
      curl -fsSL --proto '=https' --proto-redir '=https' "$APT_SOURCE_KEY_URL" \
        | gpg --dearmor -o "/usr/share/keyrings/${APT_SOURCE_NAME}-keyring.gpg"; \
      echo "$APT_SOURCE_LINE" > "/etc/apt/sources.list.d/${APT_SOURCE_NAME}.list"; \
    fi

# Install the extensions. PACKAGES is deliberately required: an image with no extensions is
# never what anyone wanted, and failing here beats shipping an empty one that makes every
# pod come up without the extension it was supposed to gain.
#
# Verify the extension directories are actually populated afterwards. A package name that
# exists but installs nothing under /usr/{lib,share}/postgresql/<major>/ (a metapackage, a
# docs-only package, the wrong major in the name) would otherwise build cleanly and produce
# an image the chart copies nothing out of -- a silently extension-less cluster, which is the
# failure mode this whole feature exists to avoid.
# Each step reports its OWN failure. An `&&` chain with one trailing `||` would blame a
# proxy/network/unsatisfiable-pin apt failure on "check PACKAGES names", which is the wrong
# thing to go looking at.
#
# BOTH trees are verified, not just the extension dir: pg.extensionPrebuiltCopyCommand runs
# `cp -n /usr/lib/postgresql/<major>/lib/*.so* /ext-lib/` FIRST and &&-chained, so a package
# set with no .so at all (a SQL-only extension, and no postgresql-<major> dependency to pull
# the server libs in) would build green and then crash-loop copy-prebuilt-ext with
# `cp: cannot stat '/usr/lib/postgresql/18/lib/*.so*'` -- exactly the build-time-silent
# failure this check exists to catch.
RUN set -eu;     if [ -z "$PACKAGES" ]; then       echo "PACKAGES is required (space-separated Debian package names)" >&2; exit 1;     fi;     apt-get update;     apt-get install -y --no-install-recommends $PACKAGES;     rm -rf /var/lib/apt/lists/*;     for d in "/usr/share/postgresql/$PG_MAJOR/extension" "/usr/lib/postgresql/$PG_MAJOR/lib"; do       if [ ! -d "$d" ] || [ -z "$(ls -A "$d" 2>/dev/null)" ]; then         echo "nothing installed under $d -- the chart copies from BOTH /usr/share/postgresql/$PG_MAJOR/extension and /usr/lib/postgresql/$PG_MAJOR/lib, and its copy command fails on an empty one. Check the PACKAGES names, that they match PG_MAJOR=$PG_MAJOR, and that the set pulls in the server libs it needs." >&2;         exit 1;       fi;     done

# Recorded so the built image can be inspected for what it actually contains, and so the
# chart's copy step has a documented contract to read from.
ENV PG_MAJOR=${PG_MAJOR}
LABEL org.opencontainers.image.title="pg-extensions" \
      org.opencontainers.image.description="Prebuilt PostgreSQL extension files for the pg/pgvector Helm charts (#320)" \
      org.opencontainers.image.source="https://github.com/cagriekin/charts" \
      io.cagriekin.pg-extensions.pg-major="${PG_MAJOR}" \
      io.cagriekin.pg-extensions.packages="${PACKAGES}"

# No ENTRYPOINT/CMD on purpose: nothing ever runs this image as a workload. The chart's
# copy-prebuilt-ext init container overrides the command with a plain `cp` and runs it as the
# unprivileged chart user -- no apt, so no root and no PSA-restricted exemption, unlike the
# in-pod install path this replaces.
