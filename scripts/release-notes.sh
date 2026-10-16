#!/bin/sh
# Prints the CHANGELOG.md section of the given version (e.g. 0.2.0).
# Fails if the section is missing: every release must have its notes.
set -eu
version=$1
awk -v v="$version" '
  /^## \[/ { if (found) exit; if (index($0, "## [" v "]") == 1) { found = 1; next } }
  found { print }
  END { if (!found) exit 1 }
' CHANGELOG.md
