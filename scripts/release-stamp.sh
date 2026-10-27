#!/bin/sh
set -eu

is_timestamp() {
  case "$1" in
    [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T*|[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]\ *|@[0-9]*)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

prefix=""
input_time=""

if [ "$#" -ge 2 ]; then
  prefix="$1"
  input_time="$2"
elif [ "$#" -eq 1 ]; then
  if is_timestamp "$1"; then
    input_time="$1"
  else
    prefix="$1"
  fi
fi

if [ -n "$input_time" ]; then
  stamp=$(TZ=Asia/Ho_Chi_Minh date -d "$input_time" +%Y%m%d-%H%M)
else
  stamp=$(TZ=Asia/Ho_Chi_Minh date +%Y%m%d-%H%M)
fi

printf '%s%s\n' "${prefix:+$prefix-}" "$stamp"
