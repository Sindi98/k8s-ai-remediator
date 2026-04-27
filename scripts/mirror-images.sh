#!/usr/bin/env sh
# mirror-images.sh — pull public images and push them to a local registry.
#
# Useful on air-gapped or bandwidth-constrained clusters (e.g. Docker Desktop
# with a local registry at host.docker.internal:5050) where pods would
# otherwise fail with ImagePullBackOff because they can't reach Docker Hub.
#
# Usage:
#   scripts/mirror-images.sh                     # mirror default list
#   REGISTRY=my-registry:5000 scripts/mirror-images.sh
#   scripts/mirror-images.sh redis:7.2-alpine    # mirror only the given images
#
# After mirroring, rewrite the workload image to point at $REGISTRY, e.g.:
#   kubectl -n ai-remediator set image deploy/ai-remediator-redis \
#     redis=host.docker.internal:5050/redis:7.2-alpine

set -eu

REGISTRY="${REGISTRY:-host.docker.internal:5050}"

DEFAULT_IMAGES="
redis:7.2-alpine
busybox:1.36
busybox:latest
polinux/stress:latest
"

if [ "$#" -gt 0 ]; then
    IMAGES="$*"
else
    IMAGES="$DEFAULT_IMAGES"
fi

printf 'Mirroring images to %s\n' "$REGISTRY"

for img in $IMAGES; do
    target="${REGISTRY}/${img}"
    printf '\n==> %s -> %s\n' "$img" "$target"
    docker pull "$img"
    docker tag "$img" "$target"
    docker push "$target"
done

printf '\nDone. Mirrored images are available at %s/...\n' "$REGISTRY"
