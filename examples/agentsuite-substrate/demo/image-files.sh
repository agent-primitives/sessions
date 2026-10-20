#!/usr/bin/env bash
# image-files.sh IMAGE DIR: the files under DIR in IMAGE's filesystem, as `tar -tv` lists them.
#
# The harness image is FROM scratch, with no shell or ls to run inside it, so this reads the image
# without running it: docker create makes a stopped container from IMAGE, docker export streams its
# filesystem, tar lists the entries under DIR, and the container is removed.
set -euo pipefail
image="${1:?usage: image-files.sh IMAGE DIR}"
dir="${2:?usage: image-files.sh IMAGE DIR}"
id="$(docker create "${image}")"
trap 'docker rm "${id}" >/dev/null' EXIT
docker export "${id}" | tar -tvf - "${dir#/}"
