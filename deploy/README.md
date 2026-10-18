# relay 部署指南（含免费资源矩阵）

`cmesh` 的架构是「双方出站 WebSocket → relay 配对转发」。要让两台位于任意
网络的机器互相连接，唯一的公网依赖就是一台双方都可达的 relay。本文给出从
零到全球可用的部署路径，以及白嫖免费资源的选项清单。

## 0. 一分钟本机验证（无任何部署）

```bash
go build -o cmesh ./cmd/cm
./cmesh relay --addr :9000 &                 # 终端 1
./cmesh recv --relay ws://127.0.0.1:9000/ws --code hello-world &  # 终端 2
./cmesh send --relay ws://127.0.0.1:9000/ws --code hello-world \
    --message "hi" --file ./some-file        # 终端 3
```

## 1. 正式部署：一台 VPS + 自动 HTTPS（推荐）

最稳的方案：任何一台有公网 IP 的机器，用 Docker Compose 起 relay + Caddy，
自动申请证书，客户端走 `wss://`（443 端口 WebSocket，与普通网页流量无异，
对防火墙和透明代理最友好）。

```bash
cd deploy
# 1) 域名 A 记录指向服务器，替换 Caddyfile 和 docker-compose.yml 里的域名
# 2) 启动
docker compose up -d
# 3) 客户端
cmesh send --relay wss://relay.example.com/ws --code <你的配对码>
```

要点：

- **relay 无状态**：配对完成后房间即销毁，重启 relay 不影响传输中的会话之外的
  任何数据；流量端到端加密，relay 读不到内容。
- **带宽就是钱**：relay 转发量 ≈ 文件大小 ×2（进 + 出）。1GB 文件消耗约 2GB
  relay 流量。选服务器时优先看流量配额而不是 CPU。
- **多地域部署**：见第 3 节，把国内 + 海外 relay 一起写进 `--relay` 列表。

## 2. 免费资源矩阵

> 以下为 2026-10 时点的公开信息，各家政策经常变，用前自行核对。
> 标注「未实测」的项指本项目作者未实际部署验证过，仅协议上可行。

### 海外（国际可达）

| 途径 | 免费额度 | 适合度 | 备注 |
|---|---|---|---|
| **Oracle Cloud Always Free** | 2 台 AMD VM + 4 OCPU ARM VM，每月 10TB 出流量 | ★★★ 最适合 | 真正长期免费的 VPS，流量配额远超需求；开机需抢注 |
| **Cloudflare Tunnel**（自家中转机） | 免费，不限 WS | ★★ 需一台源站 | 适合把没有公网 IP 的家用机变成 relay 入口；国内访问 cloudflare 质量一般 |
| **Render / Koyeb 免费层** | 单实例 Web 服务，支持 WebSocket | ★★ | Render 免费实例闲置休眠，首连延迟高；Koyeb 需信用卡验证 |
| **Fly.io** | 新账户按量计费（无纯免费层） | ★ | 曾有免费额度，现已收缩，仅列出备查 |
| **Cloudflare Workers + Durable Objects** | 每天 10 万请求 | ★★ 协议可行、未实测 | 需要把 relay 逻辑改写为 Worker（WS 双向 pipe），是社区常见的白嫖姿势；DO 免费额度内够个人用 |

### 国内

| 途径 | 免费额度 | 适合度 | 备注 |
|---|---|---|---|
| **腾讯云 / 阿里云轻量新用户** | 新购特惠（几十元/年 级别） | ★★★ 事实上最优 | 国内无长期免费 VPS，新用户特惠是性价比最高的「准免费」；备案域名可用 443 |
| **阿里云 / 腾讯云免费试用** | 1-3 个月试用 | ★★ | 适合临时用一段；到期迁移即可（relay 无状态，换服务器只是改一行 `--relay`） |
| **校园网 / 实验室内网机器** | 免费 | ★★ | 目标场景是容器互连时， Often relay 就可以直接跑在跳板机上 |
| **未备案域名 + 非标端口** | — | ★★ | 国内服务器绑域名走 443 需备案；未备案可用 IP 直连 + 自签证书，客户端 `--relay` 指向 IP:端口 |

### 一个务实的双节点组合

- **国内节点**：腾讯云/阿里云轻量（新用户特惠），跑 relay，绑定域名（备案）或 IP。
- **海外节点**：Oracle Cloud Always Free，跑 relay。
- 客户端两侧统一：

```bash
# 国内机器上
export CMESH_RELAY=wss://cn-relay.example.com/ws,wss://intl-relay.example.com/ws
# 海外机器上（同样的列表 —— 客户端按顺序找到第一台活着的 relay，
# 两端会自动在同一台上相遇；列表顺序决定了跨国传输走哪台中转）
export CMESH_RELAY=wss://intl-relay.example.com/ws,wss://cn-relay.example.com/ws
```

> 选择策略是「有序列表，第一台活的获胜」。国内↔国内的两端会停在国内 relay
> （低延迟）；国内↔海外的两端只要列表顺序一致就必然在同一台中转——把对端
> 区域的 relay 放前面可以优化跨国吞吐（例如海外对端优先走海外 relay）。

## 3. 客户端多 relay 行为（本项目已实现并测试）

- `--relay A,B,C`：按顺序尝试；连接失败或配对窗口（默认 60s，`--pair-window`
  可调）内没有对方出现，就换下一台。
- 断线自动重连：relay 或网络抖动后，两端自动重新拨号、重新跑 SPAKE2 握手，
  恢复消息与文件通道；传输中的文件从接收端 `.part` 断点续传。
- WebSocket 心跳（20s ping / 60s 超时）：NAT/透明代理静默杀连接后能在约 1
  分钟内发现并恢复，而不是永远挂死。

## 4. TLS 不可用时的降级

目标环境若只放行特定端口/域名，可退化为：

```bash
# 服务器上用 nginx/caddy 终结 TLS，或直接裸 ws（不推荐走公网明文——
# 虽然应用层已有 AES-256-GCM 端到端加密，SPAKE2 能抵抗中间人对配对码的
# 离线猜测，但裸 ws 会暴露房间号与流量特征）
./cmesh relay --addr :9000
# 客户端
cmesh send --relay ws://relay.example.com:9000/ws --code ...
```

安全性排序：`wss(公网)` > `ws(公网)` ≈ `wss(内网)` > `ws(内网)`。应用层加密
保证了即使 ws 明文传输，relay 与窃听者也读不到内容；wss 的额外价值是隐藏
流量特征、穿透只放行 443 的出口策略。

## 5. 已验证 / 未验证

- ✅ 本机多 relay 故障切换、断线重连、心跳存活检测：`bash tests/run-tests.sh`
- ✅ Docker 镜像构建：`docker build -t cmesh .`（CI 未覆盖 docker 构建，本地验证）
- ❌ 公网 VPS 真实部署与跨国链路实测：未验证（需要服务器资源）
- ❌ Cloudflare Workers 版 relay：协议可行，未实现未验证
