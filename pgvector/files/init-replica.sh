#!/bin/sh
set -e

mkdir -p "${PGBACKUP_DESTINATION}"

if [ ! -f "${PGBACKUP_DESTINATION}/standby.signal" ]; then
  until pg_isready -h "${POSTGRES_MASTER_SERVICE}" -p "${POSTGRES_PORT}" -U "${POSTGRES_USER}"; do
    sleep 2
  done

  rm -rf "${PGBACKUP_DESTINATION:?}"/*

  export PGPASSWORD="${POSTGRES_PASSWORD}"

  pg_basebackup \
    -h "${POSTGRES_MASTER_SERVICE}" \
    -D "${PGBACKUP_DESTINATION}" \
    -U "${POSTGRES_USER}" \
    -P \
    -v \
    -R \
    -X stream

  cat <<EOPGHBA > "${PGBACKUP_DESTINATION}/pg_hba.conf"
# TYPE  DATABASE        USER            ADDRESS                 METHOD

local   all             all                                     trust
host    all             all             127.0.0.1/32            trust
host    all             all             ::1/128                 trust
host    replication     ${POSTGRES_USER}        0.0.0.0/0               md5
host    all             ${POSTGRES_USER}        0.0.0.0/0               md5
EOPGHBA
fi

