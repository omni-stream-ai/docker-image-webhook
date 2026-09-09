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

For private repositories, run `podman login` or `docker login` as the user that runs the service.

The default installation is a user-level systemd service. The webhook, container engine, and post-pull command run as the current user, and no root access is required for installation. Rootless Podman is recommended when stronger separation from the host is required. Access to a system Docker socket commonly grants root-equivalent control even though the webhook process itself is not root.

## User installation

Run the installer as the user that should pull images and deploy the application. Do not use `sudo`. On the first installation, specify the repository allowed to trigger pulls:

```bash
chmod +x install.sh
./install.sh --repository ghcr.io/namespace/repo-test
```

Use `--engine docker` to switch to Docker, or `--listen 0.0.0.0:19090` to change the listen address:

```bash
./install.sh \
  --repository ghcr.io/namespace/repo-test \
  --engine docker
```

The user installation writes these files:

```text
~/.local/bin/image-pull-webhook
~/.config/image-pull-webhook/env
~/.config/systemd/user/image-pull-webhook.service
```

For Docker, the current user must be able to access the configured Docker socket. For private registries, log in as the same user before installation, for example `docker login ghcr.io`. The post-pull command and referenced Compose files must also be accessible to that user.

By default, a user service starts while that user has a login session. To keep it running after logout and start it during boot, an administrator can enable lingering once:

```bash
sudo loginctl enable-linger "$USER"
```

Running the installer again updates the program and unit but preserves the existing environment file and secret.

Common management commands:

```bash
systemctl --user status image-pull-webhook
systemctl --user restart image-pull-webhook
systemctl --user stop image-pull-webhook
journalctl --user -u image-pull-webhook -f
```

After changing `~/.config/image-pull-webhook/env`, run `systemctl --user restart image-pull-webhook`. The installer prints the generated `WEBHOOK_SECRET` only on first installation; it can later be read from that environment file.

## System installation

The previous system-level installation remains available explicitly:

```bash
sudo ./install.sh \
  --system \
  --repository ghcr.io/namespace/repo-test \
  --engine podman
```

It installs the configuration at `/etc/drawing-agent/image-pull-webhook.env` and runs as root by default. Add `--user deploy` to run the system service as an existing user. Existing system installations can continue to be upgraded by including `--system`.

`webhook.env.example` is an environment file template. Exactly one of the following must be configured:

- `WEBHOOK_ALLOWED_REPOSITORIES`: A comma-separated allowlist of image repositories, such as `ghcr.io/namespace/repo-test,registry.example.com/team/app`. The value must exactly match the image repository from a generic event. Alibaba Cloud's legacy repository path, such as `namespace/repo-test`, is also supported.
- `WEBHOOK_IMAGE_REPOSITORY`: A fixed image repository, such as `ghcr.io/namespace/repo-test`. After a valid payload is received, only the corresponding tag from this repository is pulled.
- `WEBHOOK_REGISTRY_TEMPLATE`: An optional legacy compatibility setting for Alibaba Cloud Container Registry events that provide `region` and a repository path but no image reference. Its default is `registry.%s.aliyuncs.com`.
- `WEBHOOK_POST_PULL_COMMAND`: An optional command run after a successful background pull. It is executed by `/bin/sh -c` with the full pulled image in `WEBHOOK_IMAGE`.

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

The post-pull command is trusted local configuration and is never taken from the webhook payload. Restrict write access to the service user and trusted administrators.

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

After validation, the webhook queues the image update and immediately returns `202 Accepted`:

```json
{"image":"ghcr.io/namespace/repo-test:latest","status":"accepted"}
```

Both the image pull and optional post-pull command run in the background independently of the HTTP request. Completion and failure are reported in the service journal, not in the HTTP response. Only one image update can run at a time; while either step is active, a concurrent request returns `409`. The pull and post-pull command each receive an independent timeout of 10 minutes by default, configurable with `WEBHOOK_PULL_TIMEOUT`.

This webhook only pulls images; it does not automatically restart existing containers. This prevents an external request from interrupting a live service. For automatic deployment, add an explicit restart step for a fixed Compose project after a successful pull.
