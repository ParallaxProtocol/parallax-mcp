#!/usr/bin/env bash
# Refreshes the documentation and PIPs embedded as MCP resources.
#
#   scripts/sync-content.sh [REF]
#
# Docs come from the parallax-docs repository at REF (default: main, the
# branch docs.parallaxprotocol.org is published from), read with git archive
# so the branch checked out in that clone does not matter and pages that
# exist only on development branches never ship. Every section is embedded
# except the archived client majors (client/v*.x/): client/ is the current
# client.
# PIP-0002..0005 live in GitHub Discussions and are fetched with the gh CLI;
# PIP-0006 is the networking page of the v2.0.x docs, which v2.1 renamed, read
# from the commit that imported the docs into parallax-docs.
#
# Environment: DOCS_REPO (default ../parallax-docs), GH_REPO (default
# ParallaxProtocol/parallax, whose Discussions hold the PIPs).
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
repo=${DOCS_REPO:-$here/../parallax-docs}
gh_repo=${GH_REPO:-ParallaxProtocol/parallax}
docs_github=github.com/ParallaxProtocol/parallax-docs
sections="protocol client hashwarp guides exchange-integration"
pip6_commit=d8f39d1e3d36e979b6aa8d8ef20eaaa0f2217f49
ref=${1:-main}
out=$here/pkg/resources/content

commit=$(git -C "$repo" rev-parse "$ref^{commit}")
echo "docs: $repo @ $ref ($commit)"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

rm -rf "$out/docs" "$out/pips"
mkdir -p "$out/docs" "$out/pips"
# shellcheck disable=SC2086 # sections is a word list
git -C "$repo" archive "$ref" $sections | tar -x -C "$tmp"
(cd "$tmp" && find . -name '*.mdx' -not -path './client/v*.x/*' -print0 | while IFS= read -r -d '' f; do
	mkdir -p "$out/docs/$(dirname "$f")"
	cp "$f" "$out/docs/$f"
done)
git -C "$repo" show "$pip6_commit:v2.0.x/parallax-protocol/advanced/networking/pip-0006.mdx" >"$out/pips/pip-0006.md"

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

jq -n --arg ref "$ref" --arg commit "$commit" --arg docs "$docs_github" --arg fetched "$fetched" --arg gh "$gh_repo" \
	'{docs: {repository: $docs, ref: $ref, commit: $commit}, pips: {discussions: "https://github.com/\($gh)/discussions", fetched: $fetched}}' \
	>"$out/SOURCE.json"
echo "docs: $(find "$out/docs" -name '*.mdx' | wc -l) pages; pips: $(ls "$out/pips" | wc -l)"
