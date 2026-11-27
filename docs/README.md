# 文档索引

> **每份文档都在这张表里；表里没有的，不该存在。** 新增文档必须在此登记一行（T-0.9）。
> 现状：M4 **未完成**（验收清单还没有逐格结果台账 —— 台账与工具正在 M4-3 里落地）；远端 `origin/develop` 的 CI 是绿的。详见 [`PLAN.md`](PLAN.md) §1。

## 先读这三份

| 文档 | 回答什么 |
|---|---|
| [`REQUIREMENTS.md`](REQUIREMENTS.md) | **要什么**：功能（`R-*`）、性能门槛（`P-*`）、兼容与运维（`C-*` / `O-*`）、硬边界（`N-*`）。只读基线 R3 |
| [`DESIGN.md`](DESIGN.md) | **怎么搭**：身份 → 发现 → 连接 → 路由，四层架构。只读基线 |
| [`PLAN.md`](PLAN.md) | **做到哪、接下来做什么**：现状 · M4/M5/M6 任务表 · 封口条件 · 每日节奏 · 待裁定 |

## 其余文档

| 类别 | 文档 | 回答什么 |
|---|---|---|
| 规则 | [`CONVENTIONS.md`](CONVENTIONS.md) | 纪律（`T-*` 条约）、提交前的六道门、事故记录 |
| 历史 | [`decisions.md`](decisions.md) | 为什么是这样做：产品定义的变更、技术选型、每条 `D<n>`、被否决的方案 |
| 历史 | [`ROUND-LOG.md`](ROUND-LOG.md) | 每轮发生了什么（只追加） |
| 今日 | [`project-dashboard.html`](project-dashboard.html) · [`DEV-KPI.md`](DEV-KPI.md) | 看板，以及同一份 KPI 的文字版 |
| 设计 | [`design/ARCHITECTURE-MAP.md`](design/ARCHITECTURE-MAP.md) | 代码地图：一个子命令到数据面经过什么 |
| 设计 | [`design/MESH-DESIGN.md`](design/MESH-DESIGN.md) | 组网（阶段 2）详细设计 |
| 设计 | [`design/STAGE1-PORTFORWARD.md`](design/STAGE1-PORTFORWARD.md) | 端口转发（阶段 1，已冻结）详细设计 |
| 设计 | [`design/RELAY-FRAME.md`](design/RELAY-FRAME.md) | 中转记录的二进制帧 |
| 设计 | [`design/CREDENTIAL.md`](design/CREDENTIAL.md) | 统一凭证与委派签发 |
| 设计 | [`design/M4-ACCEPTANCE.md`](design/M4-ACCEPTANCE.md) | M4 的 52 格验收清单（§2 是方法，§10 是结果台账） |
| 设计 | [`design/MULTI-HOMING.md`](design/MULTI-HOMING.md) | 一机多网（未开工，M5-16） |
| 设计 | [`design/APP-PACKAGING.md`](design/APP-PACKAGING.md) | 跨平台 App 包装（未开工，M6） |
| 部署 | [`../deploy/anchor/README.md`](../deploy/anchor/README.md) | 锚点重装后的一键重建：命令、顺序、身份为什么是最难重建的那一件 |
| 证据 | [`research/userspace-netstack.md`](research/userspace-netstack.md) · [`mux-and-portforward.md`](research/mux-and-portforward.md) · [`reliable-transport.md`](research/reliable-transport.md) · [`p2p2-kpi-log.md`](research/p2p2-kpi-log.md) | 带 `[实测]` 标注的研究：用户态网络栈（结论：不做）· 多路复用与端口转发 · 可靠传输 · **已删除的 `internal/p2p2` 的逐轮实验记录（D79）** |
| 图 | [`diagrams/`](diagrams/) | `mesh.mmd` / `mesh.png` 拓扑，`stack.mmd` / `stack.png` 协议栈，`render.sh` 渲染脚本；改图改 `.mmd` 再跑脚本 |

## 已删除的旧文档（内容已并入上表）

下表里提交过的文件最后存在于提交 `a16f3e3`，原文可用 git 查，例如
`git show a16f3e3:docs/architecture/ARCHITECTURE-SDWAN.md`。

| 旧文档 | 去了哪里 |
|---|---|
| `PROJECT-PLAN.md` · `M4-PLAN.md` · `PROJECT-ROADMAP.gpm` | 并入 `PLAN.md` |
| `req-changelog.md`（未曾入库） · `REVIEW-RESPONSE-*.md` | 并入 `decisions.md` 开头 |
| `TEAM.md` | 六道门并入 `CONVENTIONS.md` §6；报告格式并入 `ROUND-LOG.md` 开头 |
| `PM-REVIEW.md` | 产品决策与红线并入 `PLAN.md` §10 |
| `DEVELOPMENT-GAPS.md` | 仍未闭环的三块并入 `PLAN.md`：正式发布门槛（§9）、权限模型（§8）、表述红线（§10） |
| `ARCHITECTURE-SDWAN.md` | 已被 R3 产品定义取代；`design/STAGE1-PORTFORWARD.md` 仍按节号引用它 |
| `architecture/` 目录 | 与 `design/` 合并 |
| `diagrams/stage1-*` | 阶段 1 的三张图，阶段已冻结 |
