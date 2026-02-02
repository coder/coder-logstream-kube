#!/usr/bin/env bash

# This script creates a Helm package for the given version. It will output a
# .tgz file at the specified path, and may optionally push it to the Coder OSS
# repo.
#
# ./helm.sh [--version 1.2.3] [--output path/to/coder.tgz] [--push]
#
# If no version is specified, defaults to the version from ./version.sh.
#
# If no output path is specified, defaults to
# "$repo_root/build/coder_logstream_kube_helm_$version.tgz".
#
# If the --push parameter is specified, the resulting artifact will be published
# to the Coder OSS repo. This requires `gsutil` to be installed and configured.

set -euo pipefail
cd "$(dirname "$(dirname "${BASH_SOURCE[0]}")")"

log() {
	echo "$*" 1>&2
}

version=""
output_path=""
push=0

args="$(getopt -o "" -l version:,output:,push -- "$@")"
eval set -- "$args"
while true; do
	case "$1" in
	--version)
		version="$2"
		shift 2
		;;
	--output)
		output_path="$(realpath "$2")"
		shift 2
		;;
	--push)
		push="1"
		shift
		;;
	--)
		shift
		break
		;;
	*)
		error "Unrecognized option: $1"
		;;
	esac
done

if [[ "$version" == "" ]]; then
	version="$(./scripts/version.sh)"
fi

if [[ "$output_path" == "" ]]; then
	mkdir -p build
	output_path="$(realpath "build/$version.tgz")"
fi

# Make a destination temporary directory, as you cannot fully control the output
# path of `helm package` except for the directory name :/
temp_dir="$(mktemp -d)"

cd .
mkdir -p build
cp ./LICENSE build
cp -r helm/* build
log "--- Packaging helm chart for version $version ($output_path)"
helm package \
	--version "$version" \
	--app-version "$version" \
	--destination "$temp_dir" \
	./build 1>&2

log "Moving helm chart to $output_path"
cp "$temp_dir"/*.tgz "$output_path"
rm -rf "$temp_dir"

if [[ "$push" == 1 ]]; then
	log "--- Publishing helm chart..."
	# TODO: figure out how/where we want to publish the helm chart
fi
