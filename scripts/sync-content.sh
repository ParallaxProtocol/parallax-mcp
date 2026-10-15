#!/usr/bin/env bash
# Refreshes the documentation and PIPs embedded as MCP resources.
#
#   scripts/sync-content.sh [REF]
#
# Docs come from the Parallax repository at REF (default: main, the branch
# docs.parallaxprotocol.org is published from), read with git archive so the
# branch checked out in that clone does not matter and pages that exist only
# on development branches never ship.
# PIP-0002..0005 live in GitHub Discussions and are fetched with the gh CLI;
# PIP-0006 is the networking page of the v2.0.x docs.
#
# Environment: PARALLAX_REPO (default ../parallax), DOCS_VERSION (default
# v2.1.x), GH_REPO (default ParallaxProtocol/parallax).
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
repo=${PARALLAX_REPO:-$here/../parallax}
version=${DOCS_VERSION:-v2.1.x}
gh_repo=${GH_REPO:-ParallaxProtocol/parallax}
ref=${1:-main}
out=$here/pkg/resources/content

commit=$(git -C "$repo" rev-parse "$ref^{commit}")
echo "docs: $repo @ $ref ($commit), docs/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

rm -rf "$out/docs" "$out/pips"
mkdir -p "$out/docs" "$out/pips"
git -C "$repo" archive "$ref" "docs/$version" "docs/v2.0.x/parallax-protocol/advanced/networking/pip-0006.mdx" | tar -x -C "$tmp"
(cd "$tmp/docs/$version" && find . -name '*.mdx' -print0 | while IFS= read -r -d '' f; do
	mkdir -p "$out/docs/$(dirname "$f")"
	cp "$f" "$out/docs/$f"
done)
cp "$tmp/docs/v2.0.x/parallax-protocol/advanced/networking/pip-0006.mdx" "$out/pips/pip-0006.md"

fetched=$(date -u +%Y-%m-%dT%H:%M:%SZ)
gh api graphql -F owner="${gh_repo%/*}" -F name="${gh_repo#*/}" -f query='
query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    discussions(first: 100) { nodes { number title url body updatedAt author { login } } }
  }
}' --jq '.data.repository.discussions.nodes[] | select(.title | test("^PIP-[0-9]{4}"))' |
while IFS= read -r d; do
	id=$(jq -r '.title | capture("^PIP-(?<n>[0-9]{4})").n' <<<"$d")
	{
		printf -- '---\ntitle: %s\nsource: %s\nupdated: %s\nfetched: %s\n---\n\n' \
			"$(jq -r .title <<<"$d")" "$(jq -r .url <<<"$d")" "$(jq -r .updatedAt <<<"$d")" "$fetched"
		jq -r .body <<<"$d" | tr -d '\r'
	} >"$out/pips/pip-$id.md"
	echo "pip: PIP-$id from $(jq -r .url <<<"$d")"
done

jq -n --arg ref "$ref" --arg commit "$commit" --arg version "$version" --arg fetched "$fetched" --arg gh "$gh_repo" \
	'{docs: {repository: "github.com/\($gh)", ref: $ref, commit: $commit, version: $version}, pips: {discussions: "https://github.com/\($gh)/discussions", fetched: $fetched}}' \
	>"$out/SOURCE.json"
echo "docs: $(find "$out/docs" -name '*.mdx' | wc -l) pages; pips: $(ls "$out/pips" | wc -l)"
