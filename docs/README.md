# 文档索引

三层结构，**顺序不可颠倒**：需求 → 设计 → 技术。

```
① 要解决什么        REQUIREMENTS.md
        ↓
② 用什么结构解决     DESIGN.md  +  diagrams/
        ↓
③ 具体怎么做         architecture/  +  research/  +  decisions.md
        +
④ 怎么干            CONVENTIONS.md
```

---

## ① 需求层

| 文档 | 回答什么 |
|---|---|
| [`REQUIREMENTS.md`](REQUIREMENTS.md) | 谁的什么问题、功能清单（`R-*`）、非功能门槛（`P-*` / `C-*` / `O-*`）、硬边界（`N-*`）、验收标准 |

**这是唯一的「要不要做」的判据。** 没有需求编号的功能不做。

---

## ② 设计层

| 文档 | 回答什么 |
|---|---|
| [`DESIGN.md`](DESIGN.md) | 分层架构、模块划分、关键决策、数据流、部署拓扑、演进路线 |
| [`diagrams/`](diagrams/) | 三张图：加密与 mux 的顺序、谁需要入站端口、`-L` 的四道闸 |

**这是唯一的「用什么结构」的判据。** 实现与设计冲突时先改设计。

---

## ③ 技术层

### 架构细节

| 文档 | 内容 |
|---|---|
| [`architecture/ARCHITECTURE-SDWAN.md`](architecture/ARCHITECTURE-SDWAN.md) | 总体架构：分层、三种接入模式、四阶段路线、风险清单 |
| [`architecture/STAGE1-PORTFORWARD.md`](architecture/STAGE1-PORTFORWARD.md) | 阶段 1 详细设计：包结构、Go 接口、协议扩展、CLI、四层防御、测试计划（含 4 项混沌测试）、按 PR 拆分的实施路线 |
| [`architecture/REVIEW-RESPONSE-STAGE1.md`](architecture/REVIEW-RESPONSE-STAGE1.md) | 对 5 项架构质疑 + 4 项混沌测试要求的逐条答复（含没照办的两处及理由） |

### 研究报告

每份都标注结论来源（`[实测]` / `[查证]` / `[推断]`），附可复现的实验。

| 文档 | 回答的问题 | 关键结论 |
|---|---|---|
| [`research/userspace-netstack.md`](research/userspace-netstack.md) | 无 TUN 权限的容器能否获得虚拟 IP？ | **不能透明 L3**（内核无接口承载路由，`CAP_NET_ADMIN` 不可绕过）。gVisor 可提供用户态 TCP/IP，代价 +25% 体积、1.1 MiB/活跃流。**MTU 必须 ≤1200**，否则 TCP 卡死。 |
| [`research/mux-and-portforward.md`](research/mux-and-portforward.md) | 如何在一条加密连接上做多路复用与端口转发？ | 选 **yamux**（+33 KB，每流独立流控，20000 并发稳定）。**smux v1 会静默丢数据**，已否决。QUIC 原生多流吞吐低一个数量级，已否决。 |
| [`research/reliable-transport.md`](research/reliable-transport.md) | 自研可靠 UDP 传输该照抄哪套？ | 蓝本为 **KCP ARQ + ENet 消息头 + RFC 9002 丢包判定**。根因：旧实现的重组表用分片下标作 key，没有消息身份。 |

### 决策历史

| 文档 | 内容 |
|---|---|
| [`decisions.md`](decisions.md) | 按时间顺序的 D1 至今，**包含被否决的方案及原因**。提 PR 前先读与改动相关的条目。 |

### 可观测性

| 端点 | 内容 |
|---|---|
| `GET /dashboard` | 单页看板（内嵌二进制，无外部依赖，深/浅色自适应） |
| `GET /status` | 同一份数据的 JSON |
| `GET /healthz` | 存活探针 |

看板不暴露房间号、对端地址或口令——房间号是 code 的派生值，公开它等于给出可离线攻击的校验值。

---

## ④ 开发纪律

| 文档 | 内容 |
|---|---|
| [`CONVENTIONS.md`](CONVENTIONS.md) | **开发范式与条约**：三层文档纪律、提交纪律、测试纪律、代码纪律、安全纪律、每轮检查清单、事故记录 |

**这是唯一的「怎么干」的判据。** 与它冲突的做法一律视为错误，即使能跑通。

---

## ⑤ 打包与分发

[`../packaging/`](../packaging/) —— 生成可直接上传到服务器的 Demo 包：

```bash
./packaging/build-demo.sh          # 产出 dist/cmesh-demo.tar.gz
```

含 linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64 五个平台的静态二进制，
`start.sh`（自动选架构、自动挑空闲端口）、`stop.sh` 与即开即用指南。

---

## 快速导航

| 你想… | 读 |
|---|---|
| 知道这项目解决什么问题 | [`REQUIREMENTS.md`](REQUIREMENTS.md) §1–2 |
| 知道某功能做不做、做到什么程度 | [`REQUIREMENTS.md`](REQUIREMENTS.md) §3–5 |
| 理解整体架构 | [`DESIGN.md`](DESIGN.md) §1 + [`diagrams/`](diagrams/) |
| 知道某条限制为什么存在 | [`decisions.md`](decisions.md) 的 D 编号 |
| 改传输层 | [`research/reliable-transport.md`](research/reliable-transport.md) + `decisions.md` 的 D43–D46 |
| 加端口转发 / 多路复用 | [`DESIGN.md`](DESIGN.md) §3–4 + [`architecture/STAGE1-PORTFORWARD.md`](architecture/STAGE1-PORTFORWARD.md) |
| 引入 gVisor 或 TUN | [`research/userspace-netstack.md`](research/userspace-netstack.md) 的约束章节 |
| 提 PR 前自检 | [`CONVENTIONS.md`](CONVENTIONS.md) §6 检查清单 |
| 部署一台中继 | [`../packaging/README.md`](../packaging/README.md) |
| 理解多节点部署 | [`../deploy/MESH.md`](../deploy/MESH.md) |
