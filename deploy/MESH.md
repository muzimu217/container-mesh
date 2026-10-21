# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 muzimu217

# 分布式 relay 网格（mesh）

每台有公网 IP 的服务器都跑一个 relay，所有节点地位对等：既为别人中转，
也能借用别人的中转。没有中心节点。

---

## 1. 这个架构能做什么、不能做什么

### ✅ 能做到

- **任意两台机器互通**：只要它们都能出站连到**同一台** relay
- **节点互为备份**：任一台 relay 挂了，客户端自动切到下一台
- **就近接入**：客户端把延迟最低的节点排在列表前面
- **零中心依赖**：没有「主节点」，任何一台都能当入口

### ❌ 做不到

- **relay 之间自动发现**：relay 不会互相注册。列表必须由客户端提供
  （可以写进配置、环境变量，或从任意一个 HTTP 地址拉取）
- **relay 之间链式转发**：A 的数据不会经 relay1 → relay2 → B。
  两端必须在**同一台** relay 上相遇

**为什么不能链式转发**：链式转发需要在节点间建立隧道并处理环路、
分层加密、故障切换，复杂度相当于再实现一层网络协议。而实测单跳 relay
跨云已有 11-12 MiB/s，多跳只会更慢（每跳都要解密再加密）。

---

## 2. 部署：每台机器跑一个 relay

在**每台**有公网 IP 的机器上：

```bash
# 1) 准备证书（用域名时；纯 IP 可跳过，见下文）
#    有域名：用 certbot / acme.sh 申请 Let's Encrypt
#    无域名：用自签证书 + 客户端信任（见第 4 节）

# 2) 启动 relay，直接占 443
cmesh relay --addr :443 --tls-cert /path/cert.pem --tls-key /path/key.pem

# 或者无 TLS（仅内网/测试）
cmesh relay --addr :443
```

验证：

```bash
curl -k https://<该节点IP或域名>/healthz    # 应返回 ok
```

**注意**：443 已被 nginx 占用时，要么停掉 nginx，要么让 nginx 把
`/ws` 反代到 relay 的本地端口（见 `deploy/README.md`）。

---

## 3. 客户端：用同一份节点列表

两端必须用**完全相同的列表和顺序**，否则可能选到不同节点而无法配对。

```bash
export CMESH_RELAY="wss://node-a.example.com/ws,wss://node-b.example.com/ws,wss://node-c.example.com/ws"

# 接收方
cmesh recv --code "my-code" --out ./downloads

# 发送方
cmesh send --code "my-code" --file ./big.iso
```

行为：

- 按列表顺序尝试
- 连接失败或配对窗口内没等到对方 → 换下一个节点
- 全部失败 → 退避后从头再来

**为什么两端列表必须一致**：客户端各自独立选「第一个能连上的节点」。
如果 A 选节点1、B 选节点2，它们在不同的房间里，永远见不到对方。
列表一致 + 同样的故障状态，才能收敛到同一节点。

---

## 4. 无域名时的证书方案

443 端口的客户端通常不校验证书（本项目用应用层 SPAKE2 认证），
但仍建议有 TLS 以隐藏流量特征。

**方案 A：自签证书（推荐用于纯 IP 部署）**

```bash
# 生成（10 年有效，CN 用 IP）
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout key.pem -out cert.pem -days 3650 -nodes \
  -subj "/CN=<节点IP>" -addext "subjectAltName=IP:<节点IP>"

cmesh relay --addr :443 --tls-cert cert.pem --tls-key key.pem
```

客户端连 `wss://<IP>/ws`。**证书不会被验证**（QUIC/TLS 层的身份由
SPAKE2 保证），所以自签可用。

**方案 B：Cloudflare 代理（有域名时最省事）**

域名托管在 Cloudflare，开启橙色云（Proxy），源站 relay 跑明文 ws。
Cloudflare 自动提供 443 TLS 并回源。**注意**：Cloudflare 免费版对
WebSocket 有连接时长限制（约 100 秒无数据会断），但本项目有心跳
（20s ping），可以保持。

---

## 5. 节点选择策略（手动，但有效）

因为 relay 不会自动测速，节点顺序由你决定。建议：

| 两端位置 | 建议列表顺序 |
|---|---|
| 都在国内 | 国内节点优先 |
| 都在海外 | 海外节点优先 |
| 一国内一海外 | **把海外节点放前面**（国内→海外出口通常比海外→国内入口稳定） |

实测参考（腾讯云成都 ↔ 阿里云深圳，经一台中转）：
**100 MiB / 8.9 秒 / 11.3 MiB/s**。

---

## 6. 验证部署是否正确

```bash
# 1) 节点自身健康
curl -k https://<节点>/healthz

# 2) 两个客户端能否在同一节点配对
#    终端 A
cmesh recv --relay "wss://<节点>/ws" --code "test-123456" --out /tmp/rx
#    终端 B
cmesh send --relay "wss://<节点>/ws" --code "test-123456" --file /etc/hostname

# 3) 多节点容灾
#    把列表里第一个指向一个不存在的地址，验证自动切换
cmesh send --relay "wss://192.0.2.1/ws,wss://<真节点>/ws" --code ... --file ...
```

---

## 7. 已知限制

1. **两端必须选到同一节点**。列表顺序不一致或节点健康状态不同步时，
   需要一次配对窗口的等待才能收敛。
2. **没有自动测速**。节点顺序是静态的，不会按当前 RTT 动态调整。
3. **没有 relay 间链式转发**。跨洲传输必须两端都连同一节点。
4. **relay 无状态**。重启后正在进行的会话断开，客户端会自动重连。
