#!/bin/bash
set -e

VERSION=${1:?Usage: ./build.sh <version> (e.g. ./build.sh v25)}

PROD_REPO="asia-southeast1-docker.pkg.dev/revve-infra-prod/revve-repo/revve-sip"
DEV_REPO="asia-southeast1-docker.pkg.dev/revve-infra-dev/revve-dev/revve-sip"

echo "Building revve-sip:${VERSION}..."
docker build --platform linux/amd64 -t "${PROD_REPO}:${VERSION}" -f build/sip/Dockerfile .

echo "Pushing to prod repo..."
docker push "${PROD_REPO}:${VERSION}"

echo "Tagging for dev repo..."
docker tag "${PROD_REPO}:${VERSION}" "${DEV_REPO}:${VERSION}"

echo "Pushing to dev repo..."
docker push "${DEV_REPO}:${VERSION}"

echo "Done! Pushed ${VERSION} to both prod and dev."
