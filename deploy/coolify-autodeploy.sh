#!/bin/sh
# Redeploys parallax-mcp on Coolify when a new image is published.
#
# Runs on the Coolify server itself (cron or a systemd timer), so the Coolify
# API only needs to accept requests from the server: nothing calls in from
# outside. It asks GHCR for the digest of the tracked tag, without pulling
# anything, and when it changed since the last run it asks the local Coolify
# API to redeploy the resource.
#
# Configuration, in /etc/parallax-mcp-autodeploy.env or the environment:
#   COOLIFY_TOKEN   API token with deploy permission (required)
#   COOLIFY_UUID    the parallax-mcp resource UUID (required; it is the
#                   uuid= parameter of the resource's deploy webhook)
#   COOLIFY_API     default http://127.0.0.1:8000/api/v1
#   IMAGE_TAG       default latest
#   STATE_DIR       default /var/lib/parallax-mcp-autodeploy
set -eu

[ -r /etc/parallax-mcp-autodeploy.env ] && . /etc/parallax-mcp-autodeploy.env

: "${COOLIFY_TOKEN:?set COOLIFY_TOKEN}"
: "${COOLIFY_UUID:?set COOLIFY_UUID}"
COOLIFY_API=${COOLIFY_API:-http://127.0.0.1:8000/api/v1}
IMAGE_TAG=${IMAGE_TAG:-latest}
STATE_DIR=${STATE_DIR:-/var/lib/parallax-mcp-autodeploy}
REPO=parallaxprotocol/parallax-mcp

mkdir -p "$STATE_DIR"
state="$STATE_DIR/digest"

# Anonymous pull token for the public package, then the manifest digest.
token=$(curl -fsS "https://ghcr.io/token?scope=repository:$REPO:pull" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
digest=$(curl -fsSI \
	-H "Authorization: Bearer $token" \
	-H "Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json" \
	"https://ghcr.io/v2/$REPO/manifests/$IMAGE_TAG" |
	tr -d '\r' | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *//p')

if [ -z "$digest" ]; then
	echo "could not read the digest of ghcr.io/$REPO:$IMAGE_TAG" >&2
	exit 1
fi

previous=$(cat "$state" 2>/dev/null || true)
if [ "$digest" = "$previous" ]; then
	exit 0
fi
if [ -z "$previous" ]; then
	# First run: remember the current image without redeploying it.
	echo "$digest" >"$state"
	echo "tracking ghcr.io/$REPO:$IMAGE_TAG at $digest"
	exit 0
fi

echo "ghcr.io/$REPO:$IMAGE_TAG changed to $digest; redeploying"
curl -fsS -H "Authorization: Bearer $COOLIFY_TOKEN" \
	"$COOLIFY_API/deploy?uuid=$COOLIFY_UUID&force=true"
echo
echo "$digest" >"$state"
