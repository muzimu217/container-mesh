# 产品评审意见与开发交付要求（PM Review / Dev Brief）

> 本文是**产品侧评审意见与派发给开发者的交付要求**，不是代码实现状态汇报。
> 可直接作为 brief 正文派发；开发者按本文执行，完成后把结果回写
> `docs/DEV-KPI.md`（当日 KPI）与 `docs/PROJECT-PLAN.md`（路线与完成判定正典）。
> 三层文档纪律见 `docs/CONVENTIONS.md` T-0.1 ~ T-0.5：**需求 → 设计 → 技术**，顺序不可颠倒。

- 日期：2026-11-14
- 分支 / 远端 tip：`develop` / `24bbf2f`
- 状态口径：`PROJECT-PLAN.md`「项目状态口径」——代码实现 / 本地验证 / 远端可复测 / 平台验收，四层分开记账
- 约束来源：`REQUIREMENTS.md` R3、`CONVENTIONS.md` V1、`M4-PLAN.md`、`DEV-KPI.md`

---

## 1. 产品侧核查结论（本轮实际读代码，非推断）

| 能力 | 落点 | 真实状态 | 依据 |
|---|---|---|---|
| `cmesh forward -L/-R` 端口转发 + 默认拒绝白名单 | `internal/cli/forward.go:29` | 已实现 | R-4.5 / R-5.4 |
| `cmesh probe` / `doctor`（TUN ioctl、STUN、出站、退出码） | `internal/probe/probe.go`、`internal/cli/status.go:122` | 已实现 | O-8 |
| 锚点端口自检（四类判据、只报观测） | `internal/cli/portcheck.go:6` | 已实现 | M4-PLAN A3 |
| STUN 候选 + UDP 打洞（默认关闭，实验性加速） | `internal/p2p/negotiate.go:62` | 已实现 | 源码实测注释 |
| `--listen :0` 绑定后回读真实端口 | `internal/cli/discovery.go:793` | 已合入 `24bbf2f`（B-2） | 回归测试 |
| `cmesh status` / `doctor` | `internal/cli/status.go` | 本地完成，待放行 | DEV-KPI 本地门禁全绿 |
| direct / relay 分类（有端点=direct，无端点=relay） | `internal/relay/panel.go:63` | 已实现 | 源码 |
| 中继路由、路径回退、S3 全受限自动可达、增量快照 | M4-C / v1.1 | **未开始** | PROJECT-PLAN「当前未完成」 |
| LEAVE、撤销后切断活跃连接、组网直连断点续传 | — | **未开始** | DEV-KPI 本地新增开发 KPI |
| 纯出站 NAT 反拨失败 | netns | **已复现，归 v1.1 缺口** | client→anchor 10/10；anchor→client 失败 |

---

## 2. 撤回的错误口径（监督项，防止误导开发者）

以下说法曾出现在产品侧对话中，**均未核实、一律撤回**。开发者不得引用；对外表述一律以 `PROJECT-PLAN.md` / `DEV-KPI.md` + 源码为准。

| 错误说法 | 事实 |
|---|---|
| 「核心层 100% 完成、可生产级使用」 | M4 未收口，v1.1 未开始 |
| 「README 已写清 `curl \| sh` 一键安装」 | 安装器 / 组装器代码完成，真实 Release 资产的 checksum、安装、升级、卸载、回滚**未验收** |
| 「`cmesh join <密钥>` 一条命令入网」 | 实际是 `cmesh cred create` → `cmesh join --cred <串>`；B-3 仍在 worktree 待放行 |
| 「`status` / `ping` / `bench` 未实现」 | `mesh ping` 已在 `24bbf2f`；`status` / `doctor` 本地完成 |
| 「大文件传输已打通、速度跑满带宽」 | 组网直连断点续传未实现；`internal/p2p/negotiate.go` 实测注释为 1 GiB 场景直连 13 MiB/s vs 中继 512 MiB/s。**任何速率结论按 T-0.5 必须给命令、环境、数字** |
| 「两个无公网 IP 设备已完全打通」 | 纯出站 NAT 反拨失败已复现；N-9 明确对称 NAT × 对称 NAT 且网内无可达中转时无解 |

> 依 `CONVENTIONS.md` T-0.5：没有命令 / 环境 / 数字的「实测」一律标 `[推断]` 或删除。上表最后两条尤其重要；跨环境外推回环数据属 T-0.5 明列反例。

---

## 3. 产品决策（本轮定调，按此执行）

| 编号 | 决策 |
|---|---|
| **PM-1** | Web UI / Cockpit 为可选展示层，`cmesh-core` 不含前端资源；第三方 UI 只消费带版本号的 `/api/mesh` 契约 |
| **PM-2** | **基础形态不可插拔**：core profile 不可变清单（帧头 / 版本 / 握手 / mTLS 设备证书身份 / 成员-存活-公告三态与快照单调版本 / 基础流分帧 / 端点公告规则）任何 PR 不得改语义 |
| **PM-3** | 前期**只预留接口**：编译期 Go interface 注册表 + build tag；握手帧预留 `capabilities` 位图与 `min_compatible_version`，当前恒为 core profile、不解析不启用；**不实现插件加载、不引入动态库 / go-plugin / WASM** |
| **PM-4** | 主路径保持**用户态、零特权**（`probe.LevelUserspace`）；SSH 22 不是必需，主推 `cmesh run` 自研命令流，`cmesh forward` 覆盖标准 ssh / DB / Web。N-10：本地监听 <1024 端口需特权，故用 `-L 2222:...` 之类的高端口，不碰 22 |
| **PM-5** | 无公网容器默认路径 = 主动出站连锚点 + 中继；打洞与直连为可选加速（C-5 / C-3 / N-9 前提） |
| **PM-6** | 插件式协议是 M6 之后议题，本轮不排期 |
| **PM-7** | 后期可选：传输流量经**外部代理 / 已接入的 VPN 通道**送出（hub 带宽、容量有限或低流量场景）。属 C-5 连接层适配，**不进主链、不进核心、不升 R4、不给容器加权限**；产品本身不做成 VPN。前置为 v1.1 中继选路与路径回退 |

### 3.1 边界澄清（2026-11-14 已决议）：自研 VPN 不做，外部代理通道不是需求变更

此前产品侧把两件事混为一谈，把「接入外部代理」误读成「产品自研 VPN」，现更正并拆开：

**① 明确不做：自研 L3 / 虚拟网卡 / 全局虚拟 IP**

- `REQUIREMENTS.md` R3 §1.3「**不是 VPN**」明确排除透明 L3 与 subnet router / exit node 类全流量接管；N-7 排除 MagicDNS；U3 定义为「无 root、无入站端口、无 TUN」；需求里允许的只是「单服务暴露」（R-4.5 / R-5.4），已由 `cmesh forward` 满足。
- 按 `CONVENTIONS.md` T-0.1，这不是「远期候选」：**不立项、不排期、不写进里程碑**。网主已确认维持 R3，不启动需求变更（不升 R4）。将来若真要改产品定义，仍须走变更流程、`REQUIREMENTS.md` 升版（T-0.3）并在 `decisions.md` 记 `D<n>`（T-0.4）。

**② 后期可选：外部代理通道（不是需求变更）**

- 网主已确认：后期让传输流量可能走外部代理 / 已接入的 VPN 通道，用于 hub 带宽有限、低流量场景；**产品不变成 VPN**，只是接入外部通道。
- 归类：`REQUIREMENTS.md` **C-5**（传输须能穿过仅允许 HTTP(S)/WebSocket 出站的环境：代理、CDN 前置）范围内的**连接层适配**，落在 `internal/wsconn/` 一侧；不升 R4、不动 core profile、不给低权限容器加权限。
- 形态：出站流量经可配置代理地址（HTTP CONNECT / SOCKS5 / WS over 反代或 CDN），**不进主链**；本机跑 VPN 客户端建网卡需权限、U3 跑不了，不做。
- 前置：v1.1 / M4-C 中继选路与路径回退（先有选路支点，才知道何时切到外部通道）。边界：端到端加密不破，外部代理只能见密文（`DESIGN.md`「中转只转发密文」）；第三方 CDN 前置前先查 `decisions.md` 的 no-Cloudflare 结论。
- 状态：**M5 候选项、非阻塞**，不新增里程碑。

---

## 4. 给开发者的交付要求

### 4.0 前置：需求编号（T-0.1）

本节 4.2 的「一步打通 / `reachability` 字段 / 端口策略选项」属**新功能**，须先在 `REQUIREMENTS.md` 申请编号（建议 N-11 起或 R-6.x）。**本文只给产品意向与验收意向，不等于需求已立项**；无编号前只做设计，不写功能代码。

### 4.1 P0 — M4 收口（阻塞放行）

- 完成并放行当前 worktree 批次：B-3、`status` / `doctor`、面板 `308` + `no-store`、假设备 / 假遥测 / 假版本号归零、`@cat` 大输出修复、新 JS/CSS 资产、两份文档。
- 真实 GitHub Release 资产上验收 checksum、安装、升级、卸载、回滚。
- 验收：`PROJECT-PLAN.md`「M4 完成判定」四条全绿；外部 agent 在**同一新 SHA** 上复测 B-3、面板 308、真实名册、假数据归零。
- 门禁（`CONVENTIONS.md` T-1.1 ~ T-1.3）：`git status` → **先查 base 绿** → `gofmt -l .` / `go vet ./...` / `go build ./...` / `go test ./... -race -count=1` / `bash tests/run-tests.sh --quick`（41/41）/ 交叉编译 → **全绿才推**，不允许「先推上去看 CI」。
- 提交纪律 T-1.4 ~ T-1.9：一个 PR 一件事；`-L` 与 `-R` 类跨交互模型改动拆开；身份、DCO、文件纯净度、commit 拆分在首推前一次做对。

### 4.2 P1 — v1.1 / M5：一步打通与端口可达性（本轮核心待办）

按 `M4-PLAN.md` §0 范围建议，(c) 中继路由 + 增量视图推迟到 v1.1，与本节合并推进。

1. `cmesh status --json` 增加 `reachability` 分类（`direct` / `punched` / `relay-only` / `unknown`）、`listen_addr`、`observed_addr`；JSON 稳定、离线成员保留，**不发明遥测指标**（`internal/cli/status.go:21` 注释口径）。
2. 端口策略给用户选项、可强制可复现、不暗中猜：`--endpoint auto|<host:port>`、`--listen` port 0、`--port-range`、`--reachability auto|direct|relay-only`、`--no-punch`、`--allow-upnp`、手动 probe 触发。
3. 自动判定链路：绑定回读 → 自连自检 → STUN 反射地址与 NAT 类型 → **锚点 / hub 反向拨号验证**（唯一能证明「别人能连进来」的判据）→ 公告三元组 → 直连优先、失败立即降级中继。
4. 中继路由与路径回退：修复纯出站 NAT 反拨失败（netns 出站 10/10、反拨失败）；S3 全受限自动可达、增量快照、慢消费者优化。
5. 把 `probe` / `portcheck` / `p2p` / `forward` 串成单一 `up` 流程（命令名待定，见 §6）。

**验收判据**：

- 双节点均无公网 IP（netns 复现环境）：一条命令入网后 `status` 显示 `relay-only`，`mesh ping` / `msg` / 文件传输全部可用；直连可达时显示 `direct`。
- 每个判据（绑定回读 / STUN / 反拨 / 降级）都要有**独立失败用例**，不许用「全链路能通」一个测试覆盖所有分支。
- 任何速率 / 延迟结论给命令、环境、数字；禁止把回环数据外推到真实链路（T-0.5 反例）。

### 4.3 P2 — 连接生命周期与可靠传输

- LEAVE：正常退出 ≤3s、`kill -9` ≤10s 兜底、迟到 LEAVE 不误杀重连成员。
- 撤销后关闭活跃 direct QUIC、拒绝新握手、**普通版本 bump 不误杀**（R-1.3 / M4-PLAN A4）。
- 组网直连断点续传：`.part` + offset + hash；覆盖源文件变化、坏 `.part`、覆盖保护、空文件。
- **共用连接事件接口，先设计后并行实现**（`DEV-KPI.md` 并行开发队列 2），避免多个 agent 改同一路径。

### 4.4 P3 — 不排期 / 后期可选

- **明确不做**：TUN / 自研 L3 平面、全局虚拟 IP、整机流量接管（R3 §1.3 硬边界，见 §3.1）。不立项、不排期。
- **后期可选（非阻塞）**：外部代理通道，属 C-5 连接层适配，挂在 M5 / v1.1 候选项之后；不进主链、不进核心、不给容器加权限；前置为中继选路与路径回退。
- **不排期**：插件式协议，只保持 §3 的接口预留；不实现、不引入核心包。

---

## 5. 红线（监督清单，每次交付复核）

| 编号 | 红线 |
|---|---|
| **PM-R1** | 不得把方案、交叉编译、可运行写成「可安装 / 真机验收」；四层记账分开 |
| **PM-R2** | 不得在 `status` / `doctor` / 面板发明遥测或假数据；未知显示「未采集」 |
| **PM-R3** | 不得把 `probe` / `portcheck` 的「观测事实」改成猜测或静默自动修复 |
| **PM-R4** | 不得为「跑任意服务」把 TUN / 全局虚拟 IP / 整机流量接管引入核心包（违反 R3 §1.3 与 U3）；**接入外部代理通道不属此列** |
| **PM-R5** | 不得把 SOCKS5 / 外部代理通道放进**主链**；`cmesh forward` 与 `cmesh run` 才是主路径。但「后期接入外部代理通道」是 C-5 范围内的可选拓展，**不得据此一并否定** |
| **PM-R6** | 不得改 core profile 不可变清单；任何改动需产品侧重新评审 + `decisions.md` D 编号 |
| **PM-R7** | 不得在无需求编号时开工新功能（T-0.1） |
| **PM-R8** | 不得在 base 红或本地门禁未全绿时推送（T-1.1 ~ T-1.3） |

---

## 6. 待网主决策 / 放行

1. 当前 worktree 批次的提交与 `develop` 推送放行（本会话不 commit / push）。
2. 真实 GitHub Release 资产验收所需环境与发布凭据；凭据只从环境变量或密钥服务读取，源码、示例、测试都不得写入可用凭据字面量。
3. `up` 命令命名：`cmesh up` / 扩展 `cmesh join` / 其他。
4. `DEV-KPI.md` 记录的静默外部 agent（kuku / netns 一线，超 30 小时、最终通知已发）按失联结案并改派复测任务。

---

## 7. 派发与回写

- 本文可作为 brief 正文直接派发；开发者回报按现有协作渠道，结果回写 `DEV-KPI.md`（当日 KPI）与 `PROJECT-PLAN.md`（路线 / 完成判定）。
- 复测必须使用与开发者相同的 SHA；交叉复现环境给命令、环境、数字（T-0.5）。
- 产品侧复核点：每次推送前核 §5 红线；每个里程碑结束核 `PROJECT-PLAN.md` 完成判定。
