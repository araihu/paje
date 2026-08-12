#!/usr/bin/env bash
set -euo pipefail

readonly expected_version="v0.21.8"

dagger_version() {
  dagger version | awk 'NR == 1 { print $2 }'
}

if [[ "${RUNNER_ENVIRONMENT:?RUNNER_ENVIRONMENT is required}" == "github-hosted" ]]; then
  test "${RUNNER_OS:?RUNNER_OS is required}" = Linux
  case "${RUNNER_ARCH:?RUNNER_ARCH is required}" in
    X64)
      archive="dagger_v0.21.8_linux_amd64.tar.gz"
      expected_sha256="53e226c7da8fb75171e58c35759d736d961ce8b3a12db0baa7b7107954fccc5a"
      ;;
    ARM64)
      archive="dagger_v0.21.8_linux_arm64.tar.gz"
      expected_sha256="cd0df4885f2050082932b4abc5a6aad9a733f6aa4e7d8474740558517ffec4af"
      ;;
    *)
      echo "Unsupported GitHub runner architecture: ${RUNNER_ARCH}" >&2
      exit 1
      ;;
  esac
  install_dir="${RUNNER_TEMP:?RUNNER_TEMP is required}/dagger-v0.21.8/bin"
  download="${RUNNER_TEMP}/dagger-v0.21.8/${archive}"
  mkdir -p "$install_dir" "$(dirname "$download")"
  curl --fail --silent --show-error --location \
    --output "$download" \
    "https://github.com/dagger/dagger/releases/download/v0.21.8/${archive}"
  printf '%s  %s\n' "$expected_sha256" "$download" | sha256sum --check --strict
  tar --extract --gzip --file "$download" --directory "$install_dir" dagger
  chmod 0755 "$install_dir/dagger"
  printf '%s\n' "$install_dir" >> "${GITHUB_PATH:?GITHUB_PATH is required}"
  PATH="$install_dir:$PATH"
elif [[ "$RUNNER_ENVIRONMENT" != "self-hosted" ]]; then
  echo "Unsupported runner environment: ${RUNNER_ENVIRONMENT}" >&2
  exit 1
fi

command -v dagger >/dev/null
resolved_version="$(dagger_version)"
if [[ "$resolved_version" != "$expected_version" ]]; then
  echo "Dagger CLI version mismatch: got ${resolved_version:-empty}, want $expected_version" >&2
  exit 1
fi
printf 'Verified Dagger CLI %s (%s)\n' "$resolved_version" "$RUNNER_ENVIRONMENT"
