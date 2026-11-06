# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 muzimu217

# 组网部署指南（阶段 2：mesh networking）

把 N 台机器连成一张自己的网：同一身份多设备、任意两台可达、有公网的节点
当发现锚点，没公网的节点照样可达。阶段 1 的 relay 网格文档见
[`MESH.md`](MESH.md)（两者可同机共存，同一进程）。

---

## 1. 当前能力边界（诚实清单）

**能用（本指南覆盖）**：

- 组网加入/审批全流程：`init` → `account` → `invite` → `join` → `authorize`
- 成员互见：任意成员实时看到全网成员与在线状态
- 直连 RTT 测量：`cmesh mesh ping`（同机实测 p95 0.2ms，门槛 5ms）
- 直连交互通道：`cmesh dial <deviceID>`（双向认证，mTLS 设备证书钉死成员视图）
- 撤销即失效：成员除名后直连立即失败（证书钉死的是实时视图）

**还不能（路线图）**：

- 经网内节点中转兜底（PR-M4：直连不通时自动走第三台）
- 端到端加密层（PR-M4：目前直连链路靠 mTLS 设备证书认证，应用层 E2E 待接）
- 按设备名的文件传输/端口转发挂进组网（PR-M5：`cmesh forward -L 8080:nas:5432`）

---

## 2. 拓扑与角色

```
                    公网云服务器 A（推荐 1 台，可多台）
                    ┌──────────────────────────┐
                    │ cmesh serve-discovery     │
                    │  :8080 /mesh/*  发现节点   │
                    │  :8080 /ws      阶段1 relay│
                    └───────────▲──────────────┘
                                │ 出站 WebSocket（穿代理/CDN）
        ┌───────────────────────┼───────────────────────┐
        │                       │                       │
┌───────┴────────┐     ┌────────┴────────┐     ┌────────┴────────┐
│ 家里 NAS        │     │ 云服务器 B       │     │ 受限容器 C       │
│ cmesh join      │     │ cmesh join      │     │ cmesh join      │
│ --listen        │     │ --listen        │     │ （无 --listen：  │
│ 7000            │     │ 7000            │     │  只出站也可见）   │
└─────────────────┘     └─────────────────┘     └─────────────────┘
```

- **发现节点**（`serve-discovery`）：必须自己也是成员；成员名单明文在它内存里，
  所以**跑在有公网、你信得过的机器上**。阶段 1 的 relay 与它同进程同端口。
- **成员节点**（`join`）：零入站端口也能入网（C-3）；带 `--listen` 则可被
  其他成员直连。有公网的成员把端口放行；NAT 后的成员依赖打洞/中转（M4）。
- 任意成员之间：`mesh ping` / `dial` 直连，端点从成员视图自动解析，
  不需要手工配 IP。

---

## 3. 从零到三台互通（实操序列）

### 第 1 步：信任锚（一台最可信的机器）

```bash
cmesh init                # 设备身份（交互输口令）
cmesh account create      # 账号密钥（签发设备证书的根）
cmesh account device-add  # 把本设备签进账号
```

### 第 2 步：发现节点上线（有公网的机器）

```bash
cmesh serve-discovery --addr :8080 --account-pub <base64>
# --account-pub 从 `cmesh account show` 拿；提供它之后，certs 目录收编的
# 每张证书都会先验签再入列表（强烈建议提供）
```

首次运行自动自举 `~/.cmesh/members.json`（包含本设备）——这就是 O-6
「首台设备默认成为发现节点」。

### 第 3 步：成员机器入网（每台执行）

```bash
# 公网成员机器 B（可被直连）：
cmesh init
cmesh join <invite> --listen 0.0.0.0:7000
# 输出 PUBKEY 行；同时设备处于「等待批准」状态

# 受限容器 C（只出站，不可被直连）：
cmesh init
cmesh join <invite>
```

invite 从成员机器上生成：`cmesh invite --disc <发现节点地址> --disc <备节点>`。
它不含任何秘密——拿到它只是能**申请**入网（R-1.5）。

### 第 4 步：批准（账号所有者操作）

```bash
# 拷贝 joiner 打印的 PUBKEY，然后：
cmesh account authorize --pub <PUBKEY>
# 生成 <deviceID>.cert.json —— 传到发现节点：
scp <deviceID>.cert.json root@discover-host:~/.cmesh/certs/
# serve-discovery 的 watcher 秒级收编并推送，joiner 立即获批
```

### 第 5 步：验证

```bash
# 任意成员机器上：
cmesh mesh ping <B的deviceID>       # 直连 RTT
cmesh dial <B的deviceID>            # 直连交互通道（行回显）
```

`mesh ping` 输出形如：

```
seq=1 rtt=0.203ms
...
--- <deviceID>: 5/5 pings, p50 0.198ms p95 0.203ms (P-1 line: <5ms)
```

---

## 4. 容器 / Agent 部署（凭证打包即开即用）

这是「打包凭证 → 上传给某个 Agent → 它在自己容器里跑起来 → 直接通信」
的操作法。

### 4.1 哪些文件是凭证

| 文件 | 是什么 | 泄露后果 |
|---|---|---|
| `device.json` | **设备私钥**（Argon2id 加密） | 冒充该设备；立即撤销即可止损 |
| `<deviceID>.cert.json` | 设备证书（公开数据） | 无害 |
| `discovery.json` | 发现节点地址 + invite（无秘密） | 无害 |
| `members.json` | 成员列表（公开数据） | 无害 |
| `account.json` | **账号私钥** | 冒充任何设备 + 签发新设备——**绝不进容器** |

**原则：容器里只放 `device.json`（加密）+ `cert` + `discovery.json`；
`account.json` 永远留在你的信任锚机器上。**

### 4.2 容器内启动（三个环境变量搞定）

```bash
# 容器内：
export CMESH_PASSPHRASE="<打包时用的口令>"
cmesh join "$(cat /mesh/discovery.invite)" --listen 0.0.0.0:7000
```

- 口令不进镜像、不进环境文件，由编排系统注入（K8s Secret / docker secret）。
- 无入站端口的容器省略 `--listen`：它照样入网、照样能主动 `dial` 别人。

### 4.3 Docker 示例

```dockerfile
FROM alpine
COPY cmesh /usr/local/bin/cmesh
COPY state/ /mesh/state/          # device.json + cert + discovery.json
# 口令由编排系统注入环境变量（docker run -e CMESH_PASSPHRASE=... / K8s Secret），
# cmesh 会优先读 CMESH_PASSPHRASE，不进进程参数（ps 不可见）
ENTRYPOINT ["cmesh", "join", "--state", "/mesh/state", "--listen", "0.0.0.0:7000"]
```

### 4.4 systemd 示例（普通服务器）

```ini
# /etc/systemd/system/cmesh-join.service
[Unit]
Description=container-mesh member node
After=network-online.target

[Service]
Environment=CMESH_PASSPHRASE=%e/CMESH_PASSPHRASE
ExecStart=/usr/local/bin/cmesh join --listen 0.0.0.0:7000
Restart=always
RestartSec=5
User=cmesh

[Install]
WantedBy=multi-user.target
```

---

## 5. 公网 / 无公网混合实验清单

| 实验 | 期望 | 对应需求 |
|---|---|---|
| 两台都无公网（同局域网） | `mesh ping` 直连通（私网端点） | R-3.1 |
| 一台公网一台 NAT 后 | NAT 侧能 ping 公网侧（公网侧端点直连） | R-3.1 |
| 两台不同 NAT 后、都有 `--listen` | 依赖打洞；M3 尚无自动打洞编排——预期报「无可达端点」或超时，属**已知边界**（M4 中转兜底） | N-4 |
| 受限容器（只出站） | 入网、可见、能主动 ping 别人 | C-3 / S5 |
| kill -9 一个成员 | 其余成员 ≤10s 内视图更新为 offline | P-5（QUIC 10s idle 显式配置） |
| 撤销一台设备 | 其直连立即失败，其余成员不受影响 | R-1.3 |

---

## 6. 故障排查

| 现象 | 原因 | 处理 |
|---|---|---|
| `join` 卡在等待批准 | 证书未进发现节点 | 走第 4 步；确认 certs 目录 + `--account-pub` |
| `mesh ping` 报「has announced no endpoints」 | 目标没开 `--listen` | 目标重启带 `--listen` |
| `serve-discovery` 起不来报 bind 错误 | 端口被占（O-7） | 换 `--addr :8081`，或停掉占用者 |
| ping 握手失败「timestamp outside window」 | 双方时钟差 >5s | 上 NTP； skew 窗口是安全参数不要调大 |
| join 报「invite issue time is in the future」 | 本机时钟快了 >5min | 校时 |
| 中转需求（两端都不可直连） | M3 未含中转 | 等 PR-M4；或临时让两端之一挂公网 |

---

## 7. 安全须知

- **invite 无秘密**：泄露 invite 不等于泄露网络（入网要批准）。
- **设备私钥加密存储**：`device.json` 拿走也解不开（口令不在包里）。
- **撤销是瞬时武器**：任何设备可疑，账号所有者直接从成员列表除名——
  直连与发现同步失效，无需换全网密钥。
- **发现节点可拒绝服务但无法伪造**：客户端逐证书验证快照（账号公钥）+
  版本单调性（防旧快照重放复活已撤销成员）。
- **口令策略**：打包上传场景里，口令建议每设备独立、随包一次性使用，
  入网成功后可在信任锚机器重签（换证书不换设备 ID）。
