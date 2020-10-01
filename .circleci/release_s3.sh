#!/usr/bin/env bash

set -eu

VERSION=$(cat VERSION)

if s3 ls s3://please-releases/linux_amd64/$VERSION; then
  echo "Please $VERSION has already been released, nothing to do."
  exit 0
fi
echo "Releasing Please $VERSION"

find /tmp/workspace/*_amd64 -type f | xargs /tmp/workspace/release_signer

aws s3 sync /tmp/workspace/darwin_amd64 s3://please-releases/darwin_amd64/$VERSION
aws s3 sync /tmp/workspace/linux_amd64 s3://please-releases/linux_amd64/$VERSION
aws s3 sync /tmp/workspace/freebsd_amd64 s3://please-releases/freebsd_amd64/$VERSION

if [[ "$VERSION" == *"beta"* ]] || [[ "$VERSION" == *"alpha"* ]]; then
  echo "$VERSION is a prerelease, only setting latest_prerelease_version"
else
  echo "$VERSION is not a prerelease, setting latest_version and latest_prerelease_version"
  aws s3 cp VERSION s3://please-releases/latest_version
fi
aws s3 cp VERSION s3://please-releases/latest_prerelease_version
