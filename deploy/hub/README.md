# cmesh-hub：Agent 协调消息中转

给没有 GitHub 凭证（或没有入站能力）的测试 Agent 用的**纯出站消息端点**。
issue 仍是正典记录；hub 只是传输层，协调者（我）负责双向镜像。

## Agent 用法（就两行）

```bash
# 交报告
curl -sS -X POST -H "X-Hub-Token: $HUB_TOKEN" \
  --data-binary @报告.md \
  "https://<hub 地址>/hub/report?from=<agent 名>&topic=issue8"

# 领指示
curl -sS -H "X-Hub-Token: $HUB_TOKEN" \
  "https://<hub 地址>/hub/inbox?to=<agent 名>"
```

- topic = issue 编号；`from` = 自报身份（用于我回贴时署名）。
- inbox 按 `to=<agent 名>` 隔离；协调者通过 `/hub/instruct?to=X` 下发。

## 部署（官网服务器上）

```bash
# 构建（任意有 Go 1.24+ 的机器）
cd deploy/hub && CGO_ENABLED=0 go build -o cmesh-hub .

# 服务器上：token 与目录
sudo mkdir -p /var/lib/cmesh-hub && sudo chmod 700 /var/lib/cmesh-hub
echo 'HUB_TOKEN=<openssl rand -hex 32 的值>' | sudo tee /etc/default/cmesh-hub
sudo chmod 600 /etc/default/cmesh-hub

# systemd 单元 /etc/systemd/system/cmesh-hub.service
#   [Service]
#   EnvironmentFile=/etc/default/cmesh-hub
#   Environment=HUB_ADDR=127.0.0.1:8787
#   ExecStart=/usr/local/bin/cmesh-hub
#   User=cmesh-hub（专用低权用户）
#   Restart=always
sudo systemctl enable --now cmesh-hub
```

## nginx（遵守 T-4.6：只加 location，不新建 server 块）

在官网现有 server 块里加一个 location（TLS 由官网现有证书承担）：

```nginx
location /hub/ {
    proxy_pass http://127.0.0.1:8787;
    proxy_read_timeout 30s;
    client_max_body_size 300k;
    limit_req zone=hubzone burst=20 nodelay;   # 可选限流
}
```

## 安全模型

- 唯一凭证是共享 token（常量时间比较），头 `X-Hub-Token`。
- 名字（topic/to/from）限 `[A-Za-z0-9_-]{1,64}`，拒绝 `..` 与路径字符；
  存储锚定到 HUB_DIR 下单一常量文件 state.json，无变路径文件操作。
- 单请求体上限 256 KiB；每 key 消息上限 500 条（自动裁剪最旧）。
- 监听 127.0.0.1，入站 TLS 由官网 nginx 现有证书承担。
- state.json 权限 600；私钥/凭证永不入 hub（hub 只传消息）。
