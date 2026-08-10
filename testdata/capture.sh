#!/usr/bin/env bash
# capture.sh — build the hash oracle and emit golden vector rows for
# hash_test.go. Run from anywhere: ./testdata/capture.sh
set -e
cd "$(dirname "$0")"

AMALG=../references/sqlite-amalgamation-3530400
cc -O1 -I "$AMALG" hash_oracle.c "$AMALG/sqlite3.c" -o hash_oracle

./hash_oracle \
  "" \
  "61" \
  "616263" \
  "$(printf '61%.0s' {1..159})" \
  "$(printf '61%.0s' {1..160})" \
  "$(printf '61%.0s' {1..161})" \
  "$(printf '00ff%.0s' {1..500})"
