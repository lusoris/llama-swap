#!/bin/bash
# Select the highest numeric build tag for an exact llama.cpp backend prefix.
# Usage: pick_llama_tag <prefix> < versions-page.json (prints b<N>, exit 1 if none)
set -euo pipefail

pick_llama_tag() {
    local build_number
    # max_by keeps the tag text as published, so the printed tag always exists.
    build_number=$(jq -r --arg p "$1" '
        [.[] | .metadata.container.tags[]?
         | select(startswith($p + "-b"))
         | ltrimstr($p + "-b")
         | select(test("^[0-9]+$"))] | max_by(tonumber)
    ') || return 1
    if [[ "$build_number" == "null" ]]; then
        return 1
    fi
    printf 'b%s\n' "$build_number"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    pick_llama_tag "$1"
fi
