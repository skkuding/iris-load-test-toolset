#!/usr/bin/env bash
# Shared helpers for the codedang-iris-benchmark AWS workflow scripts.
#
# This file is meant to be sourced, not executed. It intentionally performs no
# AWS calls and contains no credentials. Keep it free of side effects so tests
# can source it safely.

set -euo pipefail

# Print a human-readable message to stderr so stdout stays machine-readable.
log() {
  printf '%s\n' "$*" >&2
}

# Print an error and exit non-zero.
die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# Fail unless a command is available on PATH.
require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found on PATH: $1"
}

# Return the lowercase hex SHA-256 of a file.
sha256_file() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
  else
    die "no SHA-256 tool found (need sha256sum or shasum)"
  fi
}

# Return the size of a file in bytes.
file_size() {
  wc -c <"$1" | tr -d '[:space:]'
}

# Return base64 of a lowercase hex string, for comparing AWS checksum fields.
hex_to_base64() {
  local hex="$1"
  if command -v xxd >/dev/null 2>&1; then
    printf '%s' "$hex" | xxd -r -p | base64
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c 'import base64,sys; sys.stdout.write(base64.b64encode(bytes.fromhex(sys.argv[1])).decode())' "$hex"
  else
    die "no hex-to-base64 tool found (need xxd or python3)"
  fi
}

# Require that a value is set and non-empty.
require_value() {
  local name="$1"
  local value="$2"
  [ -n "$value" ] || die "$name must be set and non-empty"
}

# Confirm yes/no unless --yes was supplied. Reads from stdin.
confirm() {
  local prompt="$1"
  local reply
  printf '%s [y/N] ' "$prompt" >&2
  read -r reply || reply=""
  case "$reply" in
    y | Y | yes | YES) return 0 ;;
    *) return 1 ;;
  esac
}
