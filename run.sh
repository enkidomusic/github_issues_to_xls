#!/usr/bin/env sh
set -eu
cd "$(dirname "$0")"
mkdir -p res
docker compose run --build --rm roadmap-generator
printf '\nDone. Check the res/ folder for the generated Excel file.\n'
