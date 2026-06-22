#!/usr/bin/env bash
set -euo pipefail

files=$(git ls-files --other --modified --exclude-standard)
if [[ -n "$files" ]]; then
  echo "The following files contain unstaged changes:"
  echo "$files"
  echo
  git --no-pager diff
  echo
  echo "Error: unstaged changes, see above for details."
  exit 1
fi
