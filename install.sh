#!/usr/bin/env bash
set -euo pipefail

readonly SERVICE_NAME="image-pull-webhook"
readonly INSTALL_BIN="/usr/local/bin/${SERVICE_NAME}"
readonly CONFIG_DIR="/etc/drawing-agent"
readonly CONFIG_FILE="${CONFIG_DIR}/${SERVICE_NAME}.env"
readonly UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

repository=""
engine="podman"
listen_addr="127.0.0.1:19090"

usage() {
  cat <<'EOF'
Usage:
  sudo ./install.sh --repository namespace/repository [options]

Options:
  --repository VALUE   Allowed repository, required on first install
  --engine VALUE       Container engine: podman or docker (default: podman)
  --listen VALUE       Listen address (default: 127.0.0.1:19090)
  -h, --help           Show this help

An existing environment file is preserved during upgrades.
EOF
}

while (($# > 0)); do
  case "$1" in
    --repository)
      [[ $# -ge 2 ]] || { echo "error: --repository requires a value" >&2; exit 2; }
      repository="$2"
      shift 2
      ;;
    --engine)
      [[ $# -ge 2 ]] || { echo "error: --engine requires a value" >&2; exit 2; }
      engine="$2"
      shift 2
      ;;
    --listen)
      [[ $# -ge 2 ]] || { echo "error: --listen requires a value" >&2; exit 2; }
      listen_addr="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ ${EUID} -ne 0 ]]; then
  echo "error: run this script as root (for example, sudo ./install.sh ...)" >&2
  exit 1
fi
if [[ "$engine" != "podman" && "$engine" != "docker" ]]; then
  echo "error: --engine must be podman or docker" >&2
  exit 2
fi
if ! command -v go >/dev/null 2>&1; then
  echo "error: go is required to build the service" >&2
  exit 1
fi
if ! command -v openssl >/dev/null 2>&1; then
  echo "error: openssl is required to generate WEBHOOK_SECRET" >&2
  exit 1
fi
if ! command -v "$engine" >/dev/null 2>&1; then
  echo "error: $engine is not installed" >&2
  exit 1
fi
if [[ ! -f "$CONFIG_FILE" && -z "$repository" ]]; then
  echo "error: --repository is required on first install" >&2
  exit 2
fi
if [[ "$repository" == *$'\n'* || "$repository" == *' '* || "$repository" == *'='* || "$repository" == *'#'* ]]; then
  echo "error: --repository contains unsupported characters" >&2
  exit 2
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
build_dir="$(mktemp -d)"
trap 'rm -rf -- "$build_dir"' EXIT

echo "Building ${SERVICE_NAME}..."
(cd "$script_dir" && go build -o "${build_dir}/${SERVICE_NAME}" .)

install -m 0755 "${build_dir}/${SERVICE_NAME}" "$INSTALL_BIN"
install -d -m 0750 "$CONFIG_DIR"
install -m 0644 "${script_dir}/${SERVICE_NAME}.service" "$UNIT_FILE"

if [[ ! -f "$CONFIG_FILE" ]]; then
  secret="$(openssl rand -hex 32)"
  install -m 0600 /dev/null "$CONFIG_FILE"
  {
    printf 'WEBHOOK_SECRET=%s\n' "$secret"
    printf 'WEBHOOK_LISTEN_ADDR=%s\n' "$listen_addr"
    printf 'WEBHOOK_CONTAINER_ENGINE=%s\n' "$engine"
    printf 'WEBHOOK_ALLOWED_REPOSITORIES=%s\n' "$repository"
  } >"$CONFIG_FILE"
  echo "Created ${CONFIG_FILE}"
  echo "WEBHOOK_SECRET=${secret}"
else
  echo "Preserved existing configuration: ${CONFIG_FILE}"
fi

systemctl daemon-reload
systemctl enable "$SERVICE_NAME"
systemctl restart "$SERVICE_NAME"

echo "Installed and started ${SERVICE_NAME}."
echo "Status: systemctl status ${SERVICE_NAME}"
echo "Logs:   journalctl -u ${SERVICE_NAME} -f"
