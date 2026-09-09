#!/usr/bin/env bash
set -euo pipefail

readonly SERVICE_NAME="image-pull-webhook"

repository=""
engine="podman"
listen_addr="127.0.0.1:19090"
install_mode="user"
service_user=""

usage() {
  cat <<'EOF'
Usage:
  ./install.sh --repository registry/namespace/repository [options]
  sudo ./install.sh --system --repository registry/namespace/repository [options]

Options:
  --repository VALUE   Allowed repository, required on first install
  --engine VALUE       Container engine: podman or docker (default: podman)
  --listen VALUE       Listen address (default: 127.0.0.1:19090)
  --system             Install a system service instead of a user service
  --user VALUE         User that runs a --system service (default: root)
  -h, --help           Show this help

User installation is the default and must be run without sudo. Existing
environment files are preserved during upgrades.
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
    --system)
      install_mode="system"
      shift
      ;;
    --user)
      [[ $# -ge 2 ]] || { echo "error: --user requires a value" >&2; exit 2; }
      service_user="$2"
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

if [[ "$engine" != "podman" && "$engine" != "docker" ]]; then
  echo "error: --engine must be podman or docker" >&2
  exit 2
fi
if [[ "$repository" == *$'\n'* || "$repository" == *' '* || "$repository" == *'='* || "$repository" == *'#'* ]]; then
  echo "error: --repository contains unsupported characters" >&2
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
if ! command -v systemctl >/dev/null 2>&1; then
  echo "error: systemd is required to install the service" >&2
  exit 1
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
build_dir="$(mktemp -d)"
trap 'rm -rf -- "$build_dir"' EXIT

if [[ "$install_mode" == "user" ]]; then
  if [[ ${EUID} -eq 0 ]]; then
    echo "error: user installation must not be run as root or with sudo" >&2
    echo "run it as the target user, or pass --system for a system service" >&2
    exit 1
  fi
  if [[ -n "$service_user" ]]; then
    echo "error: --user is only valid together with --system" >&2
    exit 2
  fi

  install_bin="${HOME}/.local/bin/${SERVICE_NAME}"
  config_dir="${HOME}/.config/${SERVICE_NAME}"
  config_file="${config_dir}/env"
  unit_dir="${HOME}/.config/systemd/user"
  unit_file="${unit_dir}/${SERVICE_NAME}.service"
  unit_template="${script_dir}/${SERVICE_NAME}-user.service"
else
  if [[ ${EUID} -ne 0 ]]; then
    echo "error: --system installation must be run as root (for example, sudo ./install.sh --system ...)" >&2
    exit 1
  fi
  service_user="${service_user:-root}"
  if ! id "$service_user" >/dev/null 2>&1; then
    echo "error: service user does not exist: $service_user" >&2
    exit 2
  fi

  install_bin="/usr/local/bin/${SERVICE_NAME}"
  config_dir="/etc/drawing-agent"
  config_file="${config_dir}/${SERVICE_NAME}.env"
  unit_dir="/etc/systemd/system"
  unit_file="${unit_dir}/${SERVICE_NAME}.service"
  unit_template="${script_dir}/${SERVICE_NAME}.service"

  service_uid="$(id -u "$service_user")"
  service_group="$(id -gn "$service_user")"
  service_home="$(getent passwd "$service_user" | cut -d: -f6)"
  protect_home="true"
  if [[ -z "$service_home" || ! -d "$service_home" ]]; then
    echo "error: service user must have an existing home directory: $service_user" >&2
    exit 1
  fi
  if [[ "$service_user" != "root" ]]; then
    protect_home="false"
  fi
  if [[ "$service_user" != "root" && "$engine" == "podman" ]]; then
    loginctl enable-linger "$service_user"
  fi

  sed \
    -e "s|@SERVICE_USER@|${service_user}|g" \
    -e "s|@SERVICE_HOME@|${service_home}|g" \
    -e "s|@SERVICE_UID@|${service_uid}|g" \
    -e "s|@PROTECT_HOME@|${protect_home}|g" \
    "$unit_template" >"${build_dir}/${SERVICE_NAME}.service"
  unit_template="${build_dir}/${SERVICE_NAME}.service"
fi

if [[ ! -f "$config_file" && -z "$repository" ]]; then
  echo "error: --repository is required on first install" >&2
  exit 2
fi

echo "Building ${SERVICE_NAME}..."
(cd "$script_dir" && go build -o "${build_dir}/${SERVICE_NAME}" .)

install -d -m 0755 "$(dirname -- "$install_bin")"
install -m 0755 "${build_dir}/${SERVICE_NAME}" "$install_bin"
install -d -m 0700 "$config_dir"
install -d -m 0755 "$unit_dir"
install -m 0644 "$unit_template" "$unit_file"

if [[ ! -f "$config_file" ]]; then
  secret="$(openssl rand -hex 32)"
  install -m 0600 /dev/null "$config_file"
  {
    printf 'WEBHOOK_SECRET=%s\n' "$secret"
    printf 'WEBHOOK_LISTEN_ADDR=%s\n' "$listen_addr"
    printf 'WEBHOOK_CONTAINER_ENGINE=%s\n' "$engine"
    printf 'WEBHOOK_ALLOWED_REPOSITORIES=%s\n' "$repository"
  } >"$config_file"
  echo "Created ${config_file}"
  echo "WEBHOOK_SECRET=${secret}"
else
  echo "Preserved existing configuration: ${config_file}"
fi

if [[ "$install_mode" == "user" ]]; then
  systemctl --user daemon-reload
  systemctl --user enable --now "$SERVICE_NAME"
  echo "Installed and started ${SERVICE_NAME} for ${USER}."
  echo "Status: systemctl --user status ${SERVICE_NAME}"
  echo "Logs:   journalctl --user -u ${SERVICE_NAME} -f"
else
  chown root:"$service_group" "$config_file"
  chmod 0640 "$config_file"
  systemctl daemon-reload
  systemctl enable --now "$SERVICE_NAME"
  echo "Installed and started system service ${SERVICE_NAME}."
  echo "Status: systemctl status ${SERVICE_NAME}"
  echo "Logs:   journalctl -u ${SERVICE_NAME} -f"
fi
