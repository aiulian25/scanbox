#!/bin/bash
# Runs a local build of Scan Box on a box over SSH, to try it before a release:
# deploy/to-box.sh mybox scanbox:1a2b3c4-5d6e7f8 (deploy/build.sh prints the tag). It copies
# the image and compose.yaml to ~/scanbox on the box, names the image in SCANBOX_IMAGE in
# ~/scanbox/.env, starts it, waits until Docker calls it healthy, and prints the one command
# that goes back to the image that ran before. To return to the published release, delete
# the SCANBOX_IMAGE line from that .env and run docker compose pull && docker compose up -d.
# The box needs Docker, the SSH user must be allowed to use it, and ~/scanbox must hold
# the secret files and .env with SCANBOX_ADDRESS and SCANNER_GID (see the README).
# shellcheck disable=SC2029 # the folder, image and container names are filled in here, on purpose
set -euo pipefail

BOX=${1:?usage: deploy/to-box.sh <ssh host> scanbox:<build>}
IMAGE=${2:?usage: deploy/to-box.sh <ssh host> scanbox:<build>}
PROJECT=$(cd "$(dirname "$0")/.." && pwd)
REMOTE_FOLDER=scanbox
CONTAINER=scanbox
HEALTH_WAIT_SECONDS=120
HEALTH_POLL_SECONDS=2

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
	echo "No image $IMAGE here; deploy/build.sh builds one and prints its tag." >&2
	exit 1
fi

docker save "$IMAGE" | gzip -1 | ssh "$BOX" 'gunzip | docker load'
ssh "$BOX" "mkdir -p ~/$REMOTE_FOLDER"
ssh "$BOX" "cat > ~/$REMOTE_FOLDER/compose.yaml" <"$PROJECT/compose.yaml"
previous=$(ssh "$BOX" "sed -n 's/^SCANBOX_IMAGE=//p' ~/$REMOTE_FOLDER/.env 2>/dev/null || true")
switch="cd ~/$REMOTE_FOLDER && touch .env && sed -i '/^SCANBOX_IMAGE=/d' .env && echo SCANBOX_IMAGE=%s >> .env && docker compose up -d"
# shellcheck disable=SC2059 # the command is a template for the image
ssh "$BOX" "$(printf "$switch" "$IMAGE")"

status=starting
for ((waited = 0; waited < HEALTH_WAIT_SECONDS; waited += HEALTH_POLL_SECONDS)); do
	status=$(ssh "$BOX" "docker inspect --format '{{.State.Health.Status}}' $CONTAINER")
	[ "$status" = healthy ] && break
	sleep "$HEALTH_POLL_SECONDS"
done
echo "$IMAGE on $BOX: $status."
if [ -n "$previous" ] && [ "$previous" != "$IMAGE" ]; then
	# shellcheck disable=SC2059 # the command is a template for the image
	echo "To go back to $previous: ssh $BOX \"$(printf "$switch" "$previous")\""
fi
[ "$status" = healthy ]
