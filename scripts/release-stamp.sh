#!/bin/sh
set -eu

stamp=$(TZ=Asia/Ho_Chi_Minh date +%Y%m%d-%H%M)
printf '%s%s\n' "${1:+$1-}" "$stamp"
