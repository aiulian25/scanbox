#!/bin/bash
# Builds Scan Box and tags the image with its build ID, the first 7 characters of the
# program's SHA-256, which the page shows, and the first 7 of the image's own ID:
# deploy/build.sh prints scanbox:<build ID>-<image ID>. The image's part tells apart builds
# of the same program, such as one on a newer base, so one that runs is never overwritten
# by the next.
# The files' newest time stands for the source's date, so the same source builds the same
# image; Docker's own timestamps are set to it. No provenance record is attached: it holds
# the time of each build, which would make every build's image differ.
set -euo pipefail

PROJECT=$(cd "$(dirname "$0")/.." && pwd)
REPOSITORY=scanbox
CANDIDATE=$REPOSITORY:candidate
BUILD_ID_LENGTH=7
PROGRAM=/usr/local/bin/scanbox

epoch=$(find "$PROJECT/go.mod" "$PROJECT/cmd" "$PROJECT/internal" "$PROJECT/Dockerfile" -type f -printf '%T@\n' | sort -n | tail -n 1 | cut -d. -f1)
# On a release tag the build is that version, followed by -dirty when files differ from the
# tag; anywhere else it says dev.
version=$(git -C "$PROJECT" describe --tags --exact-match --dirty 2>/dev/null | sed 's/^v//' || true)
SOURCE_DATE_EPOCH=$epoch docker build --build-arg SOURCE_DATE_EPOCH="$epoch" --build-arg VERSION="${version:-dev}" --provenance=false \
	--output "type=image,name=$CANDIDATE,rewrite-timestamp=true" "$PROJECT" >&2
container=$(docker create "$CANDIDATE")
id=$(docker cp "$container:$PROGRAM" - | tar -xO | sha256sum | cut -c1-"$BUILD_ID_LENGTH")
image=$(docker image inspect --format '{{.Id}}' "$CANDIDATE" | cut -d: -f2 | cut -c1-"$BUILD_ID_LENGTH")
# -v: the image declares a volume, which creating the container made; it goes too.
docker rm -v "$container" >/dev/null
docker tag "$CANDIDATE" "$REPOSITORY:$id-$image"
docker rmi "$CANDIDATE" >/dev/null
echo "$REPOSITORY:$id-$image"
