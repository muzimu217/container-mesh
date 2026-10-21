# 文档索引

## 设计文档

| 文档 | 内容 |
|---|---|
| [`architecture/ARCHITECTURE-SDWAN.md`](architecture/ARCHITECTURE-SDWAN.md) | **总体架构设计**：混合权限 Overlay SD-WAN 的分层、三种接入模式、四阶段路线、关键约束与风险清单。所有结论引用下方研究的实测数据。 |

## 研究报告

每份报告都标注了结论的来源（`[实测]` / `[查证]` / `[推断]`），并附可复现的实验代码。

| 文档 | 回答的问题 | 关键结论 |
|---|---|---|
| [`research/userspace-netstack.md`](research/userspace-netstack.md) | 无 TUN 权限的容器能否获得虚拟 IP？ | **不能透明 L3**（内核无接口承载路由，`CAP_NET_ADMIN` 不可绕过）。gVisor netstack 可提供用户态 TCP/IP，代价 +25% 体积、1.1 MiB/活跃流。**MTU 必须 ≤1200**，否则 TCP 卡死。 |
| [`research/mux-and-portforward.md`](research/mux-and-portforward.md) | 如何在一条加密连接上做多路复用与端口转发？ | 选 **yamux**（+33 KB，每流独立流控，20000 并发稳定）。**smux v1 会静默丢数据**，已否决。QUIC 原生多流吞吐低一个数量级，已否决。 |
| [`research/reliable-transport.md`](research/reliable-transport.md) | 自研可靠 UDP 传输该照抄哪套？ | 蓝本为 **KCP ARQ + ENet 消息头 + RFC 9002 丢包判定**。根因：旧实现的重组表用分片下标作 key，没有消息身份。 |

## 决策记录

[`decisions.md`](decisions.md) —— 按时间顺序记录所有技术与工程决策（D1 至今），
包含**被否决的方案及原因**。提 PR 前请先读与改动相关的条目。

## 快速导航

- **想知道某条限制为什么存在** → 先查 `decisions.md` 的 D 编号
- **想改传输层** → 读 `research/reliable-transport.md` 与 `decisions.md` 的 D43–D46
- **想加端口转发 / 多路复用** → 读 `architecture/ARCHITECTURE-SDWAN.md` 的阶段 1
- **想引入 gVisor 或 TUN** → 读 `research/userspace-netstack.md` 的约束章节
