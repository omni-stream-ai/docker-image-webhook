# Image pull webhook

A lightweight webhook that uses only the Go standard library. It accepts image-push events from any container registry, validates the secret and repository allowlist, then runs:

```text
podman pull <registry>/<repository>:<tag>
```

The container engine can be switched to Docker with an environment variable. Image pull arguments are passed directly; payload content is never executed through a shell. The optional post-pull command is executed only from trusted local configuration.

## Build and run

```bash
cd image-pull-webhook
go test ./...
go build -o image-pull-webhook .

WEBHOOK_SECRET='generate-a-long-random-secret' \
WEBHOOK_ALLOWED_REPOSITORIES='ghcr.io/namespace/repo-test' \
WEBHOOK_CONTAINER_ENGINE=podman \
./image-pull-webhook
```

By default, the service listens only on `127.0.0.1:19090`. It is recommended to expose `/payload` through an existing HTTPS reverse proxy. If it must listen publicly, set `WEBHOOK_LISTEN_ADDR=0.0.0.0:19090` and restrict the source in your firewall.

For private repositories, run `podman login` or `docker login` as the user that runs this service. The systemd example runs as root, so registry credentials must also be configured for root.

The systemd unit uses `ProtectSystem=full`, allowing Docker and Podman to manage their required state under `/var` and `/run` without manual directory overrides while keeping `/usr`, `/boot`, and `/etc` read-only.

## systemd installation

On the first installation, specify the only repository allowed to trigger pulls. Use its full image repository name for generic registry events. The script builds the program, generates a random secret, installs the systemd unit, and starts the service:

```bash
chmod +x install.sh
sudo ./install.sh --repository ghcr.io/namespace/repo-test
```

Use `--engine docker` to switch to Docker, or `--listen 0.0.0.0:19090` to change the listen address. Running the installation script again updates the program and unit, but does not overwrite the existing `/etc/drawing-agent/image-pull-webhook.env` or secret.

Common management commands:

```bash
sudo systemctl status image-pull-webhook
sudo systemctl restart image-pull-webhook
sudo systemctl stop image-pull-webhook
sudo journalctl -u image-pull-webhook -f
```

After changing the configuration, run `sudo systemctl restart image-pull-webhook`. The script prints the generated `WEBHOOK_SECRET` only after the first installation; it can later be viewed from the environment file with root privileges.

`webhook.env.example` is an environment file template. Exactly one of the following must be configured:

- `WEBHOOK_ALLOWED_REPOSITORIES`: A comma-separated allowlist of image repositories, such as `ghcr.io/namespace/repo-test,registry.example.com/team/app`. The value must exactly match the image repository from a generic event. Alibaba Cloud's legacy repository path, such as `namespace/repo-test`, is also supported.
- `WEBHOOK_IMAGE_REPOSITORY`: A fixed image repository, such as `ghcr.io/namespace/repo-test`. After a valid payload is received, only the corresponding tag from this repository is pulled.
- `WEBHOOK_REGISTRY_TEMPLATE`: An optional legacy compatibility setting for Alibaba Cloud Container Registry events that provide `region` and a repository path but no image reference. Its default is `registry.%s.aliyuncs.com`.
- `WEBHOOK_POST_PULL_COMMAND`: An optional command to run after a successful pull. It is executed by `/bin/sh -c` with the full pulled image in `WEBHOOK_IMAGE`. A command failure returns `502`; the image remains pulled, but the webhook does not report a successful deployment.

For example, create `/usr/local/bin/redeploy-my-app` with a fixed deployment command:

```bash
#!/usr/bin/env bash
set -euo pipefail
docker compose -f /srv/my-app/compose.yml up -d --force-recreate app
```

Then configure:

```env
WEBHOOK_POST_PULL_COMMAND=/usr/local/bin/redeploy-my-app
```

The post-pull command is trusted administrator configuration and is never taken from the webhook payload. Since the systemd unit runs as root, make the script root-owned and writable only by trusted administrators.

## Requests

The recommended portable event format is:

```json
{"image":"ghcr.io/namespace/repo-test","tag":"latest"}
```

An event may instead provide `registry` and a repository object with `full_name`, for example `{"registry":"registry.example.com:5000","repository":{"full_name":"team/app"},"tag":"latest"}`. A repository string is treated as a full image repository. Existing Alibaba Cloud Container Registry payloads using `push_data`, `repository.region`, and `repository.repo_full_name` remain supported.

```bash
curl -X POST 'http://127.0.0.1:19090/payload?secret=your-secret' \
  -H 'Content-Type: application/json' \
  --data-binary @payload.json
```

Successful response:

```json
{"image":"ghcr.io/namespace/repo-test:latest","status":"pulled"}
```

The health check is `GET /health`. Only one pull can run at a time; a concurrent request returns `409`. A pull runs for up to 10 minutes by default, configurable with `WEBHOOK_PULL_TIMEOUT`.

This webhook only pulls images; it does not automatically restart existing containers. This prevents an external request from interrupting a live service. For automatic deployment, add an explicit restart step for a fixed Compose project after a successful pull.
