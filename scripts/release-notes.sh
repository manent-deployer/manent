#!/bin/sh
# Stampa la sezione di CHANGELOG.md della versione indicata (es. 0.2.0).
# Fallisce se la sezione manca: ogni rilascio deve avere le sue note.
set -eu
version=$1
awk -v v="$version" '
  /^## \[/ { if (found) exit; if (index($0, "## [" v "]") == 1) { found = 1; next } }
  found { print }
  END { if (!found) exit 1 }
' CHANGELOG.md
