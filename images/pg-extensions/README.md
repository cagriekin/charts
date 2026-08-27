# Prebuilt PostgreSQL extension image (#320)

A build recipe for an image containing PostgreSQL extension files, for the `pg`/`pgvector`
charts to copy from via `postgresql.extensions.image`. There is **no published tag** — see
[Why it isn't published](#why-it-isnt-published).

## What problem it solves

With `postgresql.extensions.packages` set, the chart's `copy-base-ext` and `copy-ext` init
containers run `apt-get update` + `apt-get install` on **every pod (re)start** — `ext-lib`,
`ext-share` and `ext-extra-lib` are `emptyDir`s, so nothing is cached between restarts. That
is twice per pod, times every replica, on every crash, eviction, rolling update and scale-up.

Under a per-namespace default-deny egress policy the repeated work is not the real cost — the
**hosts** are. Every external host the install touches has to sit in the platform's baseline
allow, for every tenant, permanently. A Supabase-shaped package set needs three:

| Host | For |
|---|---|
| `apt.postgresql.org` | PGDG packages |
| `repo.pigsty.io` | `pgsodium`, `supabase_vault`, `pg_graphql`, `pg_net`, `supautils`, `wrappers`, `pgjwt`, `pgmq` — none of which PGDG ships |
| `deb.debian.org` | the general-purpose Debian archive, for one transitive `libsodium23` that neither of the above ships |

This image resolves the packages **once**, at build time. The pods then do a plain `cp`:

- **no apt on the pod-start path**, so no egress there at all
- **no root**, and therefore no PSA exemption. The apt path has to *replace*
  `postgresql.containerSecurityContext` with `runAsUser: 0` because dpkg needs it, which a
  namespace enforcing the PSA `restricted` profile rejects outright. This path keeps the
  unprivileged context, so it works where the apt path cannot run at all.

## Build

```bash
docker build -t registry.internal/pg-extensions:18-v1 \
  --build-arg PG_MAJOR=18 \
  --build-arg PACKAGES="postgresql-18-cron postgresql-18-pgvector" \
  images/pg-extensions
```

With a non-PGDG source, mirroring the chart's `extensions.aptSources` triple:

```bash
docker build -t registry.internal/pg-extensions:18-supabase \
  --build-arg PG_MAJOR=18 \
  --build-arg PACKAGES="postgresql-18-pgsodium postgresql-18-vault" \
  --build-arg APT_SOURCE_NAME=pigsty \
  --build-arg APT_SOURCE_KEY_URL=https://repo.pigsty.io/key \
  --build-arg APT_SOURCE_LINE="deb [signed-by=/usr/share/keyrings/pigsty-keyring.gpg] https://repo.pigsty.io/apt/pgsql/trixie trixie main" \
  images/pg-extensions
```

| Build arg | Required | Notes |
|---|---|---|
| `PG_MAJOR` | no (default `18`) | Must match the chart's `postgresql.majorVersion` — an extension `.so` is built against one major's server ABI and will not load into another. The default deliberately matches `images/pg-ha/Dockerfile`'s so the two cannot drift apart silently |
| `PACKAGES` | **yes** | Space-separated Debian package names. `=version` pins work as in the chart's `packages` |
| `APT_SOURCE_NAME` / `_KEY_URL` / `_LINE` | no | All three together or none. `APT_SOURCE_LINE` must carry `signed-by=` — without it the key fetch is decorative and the build installs unsigned packages as root |

The build **fails** rather than producing a quietly useless image when: `PACKAGES` is empty;
the `APT_SOURCE_*` triple is partially set; `APT_SOURCE_LINE` has no `signed-by=`; or the
install leaves `/usr/share/postgresql/<major>/extension` empty. That last one catches the
mistake that is otherwise invisible until `CREATE EXTENSION` — a package name that exists but
installs nothing for this major (a metapackage, a docs-only package, the wrong major in the
name).

## Use

```yaml
postgresql:
  majorVersion: "18"
  extensions:
    enabled: true
    image:
      repository: registry.internal/pg-extensions
      tag: "{major}-supabase"      # {major} substitutes postgresql.majorVersion
      # digest: sha256:...         # recommended for production
    extraLibs:
      - /usr/lib/x86_64-linux-gnu/libsodium.so.23
```

`packages` and `aptSources` must be **empty** — the chart refuses both alongside `image` at
render time, because both paths populate the same volumes with a no-clobber copy, so which
build of an extension actually won would be decided by init-container order rather than by
anything in the values file.

`extraLibs` still applies, reading from **this** image's filesystem. That is deliberate: the
same absolute paths work on both paths, so a working values file moves from `packages` to
`image` with no other edit.

**Adds extensions; does not upgrade one the server image already ships.** The chart's copy is
no-clobber and runs last, so anything the server images already placed in `ext-lib`/`ext-share`
wins — silently. The pgvector chart's `postgresql.image` ships `vector.so`, so a build here
carrying a newer pgvector is a no-op. Change the server image for that instead. Clobbering is
not an option: the `.so` files would overwrite a core lib with a build the running postmaster
never linked against (#302), and clobbering only the control files would split the SQL
definitions from the `.so`.

Either `tag` or `digest` is required. An untagged reference resolves to `:latest`, which for
an extension image means the `.so` files can change under a pod restart with nothing in the
release changing.

## Why it isn't published

The useful package set is per-deployment — a Supabase-shaped set looks nothing like a PostGIS
one — so there is no canonical tag worth publishing. Build it with your own list in your own
CI and push it to your own registry. That is also where the egress belongs: once, at build
time, instead of on every pod start in every tenant namespace.

## Contract with the chart

The chart's `copy-prebuilt-ext` init container reads exactly these paths:

| Path | Copied to |
|---|---|
| `/usr/lib/postgresql/<major>/lib/*.so*` | `ext-lib` |
| `/usr/share/postgresql/<major>/extension/*` | `ext-share` |
| each `extraLibs` entry | `ext-extra-lib` |

All three with `cp -n` (no-clobber), and the container runs **last** of the three extension
init containers. `copy-base-ext` populated `ext-lib`/`ext-share` from the image that actually
*runs* the server, and this is an independent build that can sit on a different postgres point
release — an unconditional copy would overwrite a core lib (e.g. `libpqwalreceiver.so`) with
one the running postmaster never linked against (#302). Adding only what is missing is the
whole job.

`test/dockerfile-test.sh` asserts that contract from the image side, including that these
paths still match `pg.extensionPrebuiltCopyCommand` in `pg/templates/_helpers.tpl`.

## Tests

```bash
bash images/pg-extensions/test/dockerfile-test.sh    # static, no build
```

CI additionally builds the image for both supported majors and runs the chart's own copy
command verbatim against the result (`.github/workflows/pg-ha-image-test.yaml`).
