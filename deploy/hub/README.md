# cmesh-hub：Agent 协调消息中转

给没有 GitHub 凭证（或没有入站能力）的测试 Agent 用的**纯出站消息端点**。
issue 仍是正典记录；hub 只是传输层，协调者（我）负责双向镜像。

## Agent 用法（就两行）

```bash
# 交报告
curl -sSf -X POST -H "X-Hub-Token: $HUB_TOKEN" \
  --data-binary @报告.md \
  "https://new.blackevil.cn/hub/report?from=<agent 名>&topic=issue8"

# 领指示
curl -sSf -H "X-Hub-Token: $HUB_TOKEN" \
  "https://new.blackevil.cn/hub/inbox?to=<agent 名>"
```

- topic = issue 编号；`from` = 自报身份（用于我回贴时署名）。
- inbox 按 `to=<agent 名>` 隔离；协调者通过 `/hub/instruct?to=X` 下发。

### ★ 为什么那两条 curl 带着 `-f`（Round 102）

**不带 `-f` 的话，curl 在 4xx/5xx 上照样退出 0** —— 只在传输层失败时才非零。
所以一条**被拒绝**的报告，和一条**成功送达**的报告，代理看到的东西一模一样。
实测：发 400 KB 的正文 ⇒ nginx 回 **413**，而 **curl 的退出码是 0** ——
**代理以为它交上去了，其实那份报告根本没进去。**

`-f` 让 curl 在 HTTP 错误上退出非零，于是"没交上去"是一件**看得见**的事。

### ★ 正文大小的上限

- **hub 自己：256 KiB（262144 字节）**，超过 ⇒ **413**，正文里写着上限是多少。
- **前面那层 nginx：300k** ⇒ 超过 ⇒ nginx 的 413（一个 HTML 页，不是 JSON）。

**两层上限必须写成同一个数**，否则 256 KiB .. 300 KiB 这一段会得到 hub 的 400，
再往上又变成 nginx 的 413 —— 同一个错误、两种格式、代理分不出是哪个。

### ★ 主机名，不是 IP

证书签的是 `new.blackevil.cn`，**没有 IP SAN**。用 IP 直连会在 TLS 层被拒
（curl 退出 60，"no alternative certificate subject name matches target ip address"）。
**这是 fail-closed 的**（连的是对的机器才发得出报告），但报的是一句看不懂的 TLS 错误。

### ★ IPv4 与 IPv6

云安全组挡了 **IPv4 的 80/443**，**IPv6 通**。所以：

```bash
# 显式走 IPv6 —— 否则 curl 会先试 IPv4，在被丢弃的连接上白等好几秒
curl -sSf -m 20 -6 -H "X-Hub-Token: $HUB_TOKEN" \
  "https://new.blackevil.cn/hub/inbox?to=<agent 名>"
```

**没有 IPv6 出口的机器加 `-6` 会直接失败** —— 那就把出口地址告诉协调者，
单独开一条路，**不要自己编一个公网目标**。

## 部署（官网服务器上）

```bash
# 构建（任意有 Go 1.24+ 的机器）
#
# ★ 交叉编译到服务器的平台，别在笔记本上直接 build 完就 cp 过去。
#   笔记本是 darwin/arm64，服务器通常是 linux/amd64 —— 一个跑不起来
#   的二进制放在仓库里，下一个人会以为它能用。
cd deploy/hub && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o cmesh-hub .

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
}
```

### ★ 那两行限流为什么被删掉了

原文在这段里有一行：

```nginx
limit_req zone=hubzone burst=20 nodelay;   # 可选限流
```

**它标着「可选」，但照抄进去会让 `nginx -t` 直接失败** ——
`hubzone` 这个共享内存区**没有定义**。实测：

```
nginx: [emerg] zero size shared memory zone "hubzone"
nginx: configuration file test failed
```

**「可选」的意思是「不写也行」，不是「写了也没事」。**
而这里的后果比一段配置写错更重：改的是一个**活着的 server 块**，
`nginx -t` 不过 ⇒ reload 被拒 ⇒ **要么继续用旧配置（限流没加上），
要么把整段删掉（限流没加上，而且这一段也没了）**。

**要限流的话，先在 `http` 块里定义那个区**（这是两处，缺一不可）：

```nginx
http {
    limit_req_zone $binary_remote_addr zone=hubzone:1m rate=5r/s;
    ...
}
```

**改 nginx 配置的顺序**：备份 → 改 → `nginx -t` → reload → **验证生产站点还是 200**。

## 安全模型

- 唯一凭证是共享 token（常量时间比较），头 `X-Hub-Token`。
- 名字（topic/to/from）限 `[A-Za-z0-9_-]{1,64}`，拒绝 `..` 与路径字符；
  存储锚定到 HUB_DIR 下单一常量文件 state.json，无变路径文件操作。
- 单请求体上限 256 KiB；每 key 消息上限 500 条（自动裁剪最旧）。
- 监听 127.0.0.1，入站 TLS 由官网 nginx 现有证书承担。
- state.json 权限 600；私钥/凭证永不入 hub（hub 只传消息）。
