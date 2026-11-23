#!/usr/bin/env bash
# One update attempt. Only fixed diagnostic codes go to stdout; never credentials
# or arbitrary HTTP response text. The caller decides whether to retry.
set -Eeuo pipefail
umask 077

domain=$1
token_file=$2
if [[ ! -f "$token_file" || ! -r "$token_file" ]]; then
  printf 'TOKEN_MISSING\n'
  exit 0
fi
stage=$(mktemp -d)
trap 'rm -rf -- "$stage"' EXIT
# curl's name@file syntax includes all bytes, including CR/LF from legacy files.
tr -d '[:space:]' < "$token_file" > "$stage/token"
if [[ ! -s "$stage/token" ]]; then
  printf 'TOKEN_EMPTY\n'
  exit 0
fi
if http_code=$(curl -4 -sS --connect-timeout 10 --max-time 30 --get \
  --data-urlencode "domains=${domain%.duckdns.org}" \
  --data-urlencode "token@$stage/token" \
  --output "$stage/reply" --write-out '%{http_code}' \
  https://www.duckdns.org/update 2>/dev/null); then
  :
else
  code=$?
  printf 'CURL:%s\n' "$code"
  exit 0
fi
if [[ "$http_code" != 200 ]]; then
  printf 'HTTP:%s\n' "$http_code"
  exit 0
fi
reply=$(tr -d '[:space:]' < "$stage/reply")
case "$reply" in
  OK)
    # Certbot hooks must use the same normalized credential that just succeeded.
    if ! cmp -s "$stage/token" "$token_file"; then
      install -m 0600 "$stage/token" "$token_file.next"
      mv -f -- "$token_file.next" "$token_file"
    fi
    printf 'OK\n'
    ;;
  KO) printf 'KO\n' ;;
  *) printf 'UNEXPECTED\n' ;;
esac
