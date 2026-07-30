# Image pull webhook

一个仅使用 Go 标准库的轻量 webhook。接收阿里云容器镜像服务的构建完成事件，校验密钥和仓库白名单后执行：

```text
podman pull registry.<region>.aliyuncs.com/<namespace>/<name>:<tag>
```

也可以通过环境变量切换为 Docker。程序直接传递命令参数，不会通过 shell 执行 payload 内容。

## 构建和运行

```bash
cd image-pull-webhook
go test ./...
go build -o image-pull-webhook .

WEBHOOK_SECRET='生成一个足够长的随机密钥' \
WEBHOOK_ALLOWED_REPOSITORIES='namespace/repo-test' \
WEBHOOK_CONTAINER_ENGINE=podman \
./image-pull-webhook
```

默认只监听 `127.0.0.1:19090`，建议由已有的 HTTPS 反向代理暴露 `/payload`。若确实要直接监听公网，可设置 `WEBHOOK_LISTEN_ADDR=0.0.0.0:19090`，并在防火墙中限制来源。

如果仓库是私有的，需要先用运行该服务的用户执行 `podman login` 或 `docker login`。systemd 示例以 root 运行，因此登录凭据也必须配置在 root 用户下。

安装 systemd 服务：

```bash
sudo install -m 0755 image-pull-webhook /usr/local/bin/image-pull-webhook
sudo install -d -m 0750 /etc/drawing-agent
sudo install -m 0600 webhook.env.example /etc/drawing-agent/image-pull-webhook.env
sudo install -m 0644 image-pull-webhook.service /etc/systemd/system/image-pull-webhook.service
sudo editor /etc/drawing-agent/image-pull-webhook.env
sudo systemctl daemon-reload
sudo systemctl enable --now image-pull-webhook
```

`webhook.env.example` 是环境文件模板。必须设置以下二者之一：

- `WEBHOOK_ALLOWED_REPOSITORIES`：逗号分隔的 payload 仓库白名单，例如 `namespace/repo-test,namespace/another-repo`。镜像地址由 payload 的 region 和仓库名生成。
- `WEBHOOK_IMAGE_REPOSITORY`：固定完整镜像仓库，例如 `registry.cn-hangzhou.aliyuncs.com/namespace/repo-test`。收到合法 payload 后只会拉取这个仓库对应的 tag。

## 请求

```bash
curl -X POST 'http://127.0.0.1:19090/payload?secret=你的密钥' \
  -H 'Content-Type: application/json' \
  --data-binary @payload.json
```

成功返回：

```json
{"image":"registry.cn-hangzhou.aliyuncs.com/namespace/repo-test:latest","status":"pulled"}
```

健康检查为 `GET /health`。同一时间只允许一个 pull；已有任务运行时返回 `409`。pull 默认最多运行 10 分钟，可用 `WEBHOOK_PULL_TIMEOUT` 调整。

这个 webhook 只负责拉取镜像，不会自动重启现有容器。这样可以避免一次外部请求直接中断线上服务；如需自动部署，应在 pull 成功后增加明确的、针对固定 compose 项目的重启步骤。
