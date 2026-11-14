# container-mesh 项目总规划与交付口径

更新时间：2026-11-14。当前分支：`develop`。待推送 tip：`c530c7d`。

## 项目状态口径

项目分成四个交付层级，每层单独记账：

1. **代码实现**：功能是否在源码中完成。
2. **本地验证**：完整门禁和真实本地/回环环境是否通过。
3. **远端可复测**：是否已经进入 `origin/develop`，外部 agent 能否拉到同一 SHA。
4. **平台验收**：是否在目标系统安装、运行、入网、传输、退出和恢复。

只有第 3 层之后的结果才派给远程 agent 复测。交叉编译只证明目标二进制可以生成，不等于平台安装包或真机验收完成。

产品侧评审意见、开发交付要求与红线见 `docs/PM-REVIEW.md`（可直接作为开发者 brief 派发）。本文件仍是路线与完成判定的文字正典。

## 当前已完成

### M4-A：统一凭证与一键入网

- 凭证公开半与委派记录统一为同一结构和同一签名。
- `cmesh cred create` 生成带有效期、发现地址和设备额度的凭证。
- `cmesh join --cred <串>` 自动创建身份、本机签委派证书、先准入再公告。
- 委派证书在快照中依靠委派记录验签。
- B-1：委派成员可拨，外部实测双向 10/10。
- B-2：`--endpoint host:0` 解析为实际监听端口。
- B-3：凭证设备入网后的 `mesh ping/msg` 配置读取已修复；本地真实二进制验证 ping 5/5、msg delivered 1。
- e2e 已纳入第 11 节，当前门禁 41/41。

待推送 tip `c530c7d` 已包含 B-1/B-2 和基础凭证实现；B-3 最终修复与本地面板收口仍在本地批次，推送后派外部复测。

### M4-B：端口自检与真实驾驶舱

- 锚点启动输出 TCP/UDP 同端口自检和可执行防火墙动作。
- `/api/mesh` 真实名册接口，面板令牌只认 `X-Panel-Token`。
- `/panel-lite` 轻量页面。
- `/panel` React + Vite + Three.js 驾驶舱由 Go `embed` 提供。
- 页面包含 3D 名册、在线/离线/端点/传输四维状态、过期快照和真实错误状态。
- 已清理原型中的五态演示场景、模拟按钮、两台香港假设备、假遥测、假拓扑和浏览器伪造在线状态。
- `/panel` 无尾斜杠白屏修复为 `308 → /panel/`，重定向 `no-store`。
- 生产面板动作统一改为只读提示；真实操作使用 CLI。
- 设计规范页中的示例表已标记为示意，假版本号 `v42/v43` 已清除；`spec-state-matrix-06` 用于 DOM 作用域验收。

当前面板本地构建已通过；新的 JS/CSS 构建资产已同步到 `internal/relay/cockpit-dist/`，仍待放行推送。

### M4-D：二进制、安装器与 README

- Go 交叉编译和 `tools/mkrelease` 组装器已完成。
- `latest` 别名、归档和 `SHA256SUMS` 逻辑已完成。
- `packaging/install.sh` 和 `tools/cmesh-install` 已入本地工作树。
- README 命令级错误已修正：口令旗标、参数顺序、凭证路径、relay 参数和发布件数字。
- 真实 GitHub Release 资产、签名、升级、卸载仍未验收。

### 本地新增运维线

- `cmesh status`：读取已验证 snapshot，输出成员、版本、在线状态、端点、`last_seen` 和保守 reachability。
- `cmesh mesh ping`：100ms 最小采样间隔、最多 1000 包、丢包统计、p50/p95，direct/degraded/unknown 不伪造。
- `cmesh bench`：默认每方向 100MB，独立 direct QUIC 测试流，支持 rate 限制，不写业务文件；QUIC 重传计数不可观测时明确输出 `n/a`。
- `cmesh doctor`：复用现有 probe 能力与退出码。
- `@cat` 大输出：修复 ACK reader 的 4 KiB stream 截断，新增长输出 QUIC 回归测试。

## 当前未完成

### M4-C / v1.1：中继路由与增量视图

尚未开始。包括中继选路、路径切换、全受限 S3 自动可达、增量快照和慢消费者协议优化。纯出站 NAT 已实测：客户端主动拨锚点可用，锚点反拨纯出站客户端失败，这是当前中继/路径回退缺口，不能记为凭证缺陷。

### 连接生命周期

- 显式 LEAVE 尚未实现。
- 撤销后关闭已建立 direct QUIC 连接尚未实现。
- 当前可阻止新握手，异常断连有快速 offline 路径和 heartbeat 兜底。

### 组网直连断点续传

relay 配对通道已有恢复能力；组网直连的 `.part`、offset、hash 协议尚未实现。

## 平台包装路线

| 平台/形态 | 当前状态 | 下一验收 |
|---|---|---|
| Linux headless | 核心 CLI/relay 可运行，静态二进制可生成 | systemd、服务用户、日志、卸载、升级回滚、重启恢复 |
| Windows CLI | amd64 交叉编译通过 | Windows 实机入网、传输、退出、休眠恢复 |
| macOS CLI | amd64/arm64 交叉编译通过 | 本机链路、`.app`、签名、公证、钥匙串 |
| Linux desktop | 仅方案 | 选定发行版、Wails 宿主、WebKitGTK 依赖、安装包 |
| Windows desktop | 仅方案 | Wails + WebView2、MSI/卸载、托盘、签名 |
| macOS desktop | 仅方案 | Wails `.app`、文件权限、签名、公证、休眠恢复 |
| Android | 仅 gomobile 方案 | 最小 binding、文件 URI、前台服务、真机一条链路 |
| iOS | 仅 gomobile 方案 | 最小 binding、沙箱/Keychain、真机一条链路 |
| NAS | Linux/容器能力可复用 | 目标厂商矩阵、持久化、服务编排、升级回滚 |
| router/OpenWrt | 设计方向 | target/架构、BusyBox/procd、资源和断电恢复 |

## 网络平面、端口可达性与扩展点预留（路线约束）

本节记录边界与路线判断，**不计入 M4 完成判定**。凡标「候选」者均为方案，不等于代码实现、也不等于平台验收。

### 核心 profile 不可变清单

以下属于基础形态，任何 PR 不得改动语义；新增能力只能以可选、可降级的方式叠加：

- 线格式帧头与 magic、协议版本与最低兼容版本字段。
- 握手、密钥派生、mTLS 设备证书身份与委派验签。
- 成员 / 存活 / 公告三态语义、快照单调版本、凭证与委派记录结构。
- 基础流类型与分帧（msg / file / run / ping / forward preamble）。
- 端点公告规则：`--endpoint` 是对外可拨地址，`--listen` 是绑定地址，port 0 解析为实际绑定端口；无端点成员可见但不可拨。

**基础形态不可插拔。** 插件式协议是 M6 之后的远期议题：当前不实现插件加载、不引入动态库、不引入 go-plugin / WASM。

### 前期只预留接口，不开插件

随 M5 任务零成本落地，不单独设里程碑：

- 端口分配、可达性探测、选路决策、连接事件写成 Go interface + 编译期注册表，默认实现唯一；build tag 控制可选组件（含 UI 体积裁剪）。
- 握手帧预留 `capabilities` 位图与 `min_compatible_version`，当前恒为 core profile，不解析、不启用。
- 连接事件接口与 LEAVE / 撤销切断共用（见 `DEV-KPI.md` 并行开发队列 2），避免两套漂移实现。
- 第三方 UI 只依赖带版本号的 `/api/mesh` 契约，不读内部结构。

### 端口与可达性：已有能力，不重复建设

- `cmesh forward -L/-R`：本地端口 ↔ 对端服务转发；默认拒绝，`--allow-host` / `--allow-port` / `--allow-listen-port` 白名单，`--accept` / `--accept-reverse` 开关。SSH 类场景无需 TUN：本地监听口 → mesh 流 → 对端目标端口。
- `cmesh join --endpoint/--listen`：`--listen` port 0 自动绑定并回读实际端口公告（B-2 已验收）；显式 `--endpoint` 优先。
- `cmesh probe` / `cmesh doctor`：TUN 真实 ioctl、capability、UDP/TCP 出站、DNS、localhost listen、STUN 反射地址与 NAT 判定，JSON + 退出码。
- 锚点端口自检（`internal/cli/portcheck.go`）：区分已监听 / 防火墙丢弃 / 仅 loopback / UDP 未绑 / 公告地址不可达，且只报观测事实。
- `internal/p2p`：STUN 候选 + UDP 打洞，默认关闭（实验性加速器），可把 punched socket 交给 QUIC 复用。
- `cmesh status`：成员、端点、版本、`last_seen`、reachability，JSON 稳定输出。当前 reachability 在没有实际 transport probe 时保守为 `unknown`，不把端点候选伪装成直连。

### 缺口（归 M4-C / v1.1，不混入 v1.0 收口）

- 纯出站 NAT 反拨失败已复现（netns 实测出站 10/10、锚点反拨失败）：需要中继路由与路径回退。
- S3 全受限自动可达、增量快照、慢消费者协议优化尚未开始。
- 现有 probe / portcheck 给出观测与人工动作，**尚未串成一条自动链路**；`status` 已有 reachability 字段，但 relay-only 只有在真实 relay session/transport 证据可用时才填充，当前 snapshot 默认 `unknown`。

### 一步打通与端口选择（候选，未实现）

目标：一条命令完成入网与局域网 / 无公网容器打通，端口策略可配置、可观测、可复现。

1. 给用户选项，不暗中猜：`--endpoint auto|<host:port>`、`--listen` port 0、`--port-range`（候选范围）、`--reachability auto|direct|relay-only`、`--no-punch`、`--allow-upnp`。
2. 自动判定链路：本地绑定回读 → 自连自检 → STUN 反射地址与 NAT 类型 → **锚点反向拨号验证**（唯一能证明「别人能连进来」的判据）→ 公告 `listen_addr` / `observed_addr` / reachability class → 直连优先、失败立即降级中继。
3. 把 probe / portcheck / p2p / forward 串成单一 `up` 流程，结果进 `cmesh status` JSON，供脚本消费。
4. 无公网容器默认路径：主动出站连锚点 + 中继；打洞与直连是可选加速，不是前置条件。

### 明确不做：自研 L3 / 虚拟网卡 / 全局虚拟 IP（R3 硬边界）

- `REQUIREMENTS.md` R3 §1.3「**不是 VPN**」与 N-7 明确排除透明 L3 / subnet router / exit node / MagicDNS；U3 定义为无 root、无入站端口、无 TUN。
- 产品**不自研** TUN 虚拟网卡、全局虚拟 IP、整机流量接管。按 `CONVENTIONS.md` T-0.1，这不是「远期候选」：**不立项、不排期、不写进里程碑**；任何重新提出须先走需求变更流程并升版 `REQUIREMENTS.md`。
- `probe` 已能实测 TUN 是否可用，但产品主路径仍是 `userspace_only`；能力探测用于告知「此路不通」，不是为了启用它。
- 参考对象（仅供了解边界，不作为实现清单）：Tailscale userspace networking / kernel-vs-netstack subnet routing、quic-go 可插拔拥塞控制与 connection migration、PostgreSQL 扩展与 hook、libp2p 可组合 transport / muxer / security。

### 后期可选：外部代理通道（C-5 范围内，连接层适配）

- 含义：**接入**一个已有的外部代理 / VPN 通道，让传输流量在 hub 带宽、容量有限或低流量场景下经该通道送出；产品本身**不变成 VPN**，不建网卡、不发全局 IP、不接管整机流量。
- 归类：`REQUIREMENTS.md` **C-5**（传输须能穿过仅允许 HTTP(S)/WebSocket 出站的环境：代理、CDN 前置）范围内的**连接层适配**，落在 `internal/wsconn/` 一侧；**不是需求变更，不升 R4，不动 core profile，不给低权限容器加权限**。
- 形态边界：出站流量经可配置代理地址（HTTP CONNECT / SOCKS5 / WS over 反代或 CDN）；**不进主链**，`cmesh run` / `cmesh forward` / 中继仍是主路径。涉及本机跑 VPN 客户端建网卡的需权限、U3 跑不了，不做。
- 前置：v1.1 / M4-C 的中继选路与路径回退（先有选路支点，才知道何时切到外部通道）。
- 边界：端到端加密不破，外部代理只能见密文（`DESIGN.md`「中转节点只转发密文」）；若走第三方 CDN 前置，先查 `decisions.md` 的 no-Cloudflare 结论。
- 状态：**M5 候选项、非阻塞**，不属于 M4 收口，也不单独新增里程碑。

### 里程碑归属

- **M4（当前收口）**：不含本节任何候选项，只做凭证、面板、安装器与本地运维线。
- **M5 / v1.1**：中继路由、路径回退、S3 自动可达、一步打通与端口策略、连接生命周期、断点续传；外部代理通道作为**后置候选项**挂在本阶段之后，不阻塞前述交付。
- **M6 之后**：扩展点正式开放、插件式协议预研；未立项前只保持接口预留。

## 里程碑

### M4：当前收口

目标：统一凭证 + 一键入网、端口自检 + 驾驶舱、二进制 + 安装器 + README。

完成判定：

- 代码实现和本地门禁全绿。
- 修复批次进入 develop。
- 外部 agent 在新 SHA 上复测 B-3、面板 308、真实名册、假数据归零。
- 发布件在真实 Release 资产上完成 checksum、安装、升级和回滚验证。

### M5：多网与可靠传输

候选项：一机多网、多租锚点、组网直连断点续传、LEAVE、撤销会话切断、权限和后台生命周期完善。

### M6：平台与生产化

候选项：Wails 桌面首包、Android/iOS 最小宿主、NAS/router 交付矩阵、签名分发、升级回滚、真实 WAN/NAT 生产验收。

## 每日 KPI 口径

每天更新 `docs/project-dashboard.html`，至少包含：

- 当日开发完成项与未完成项。
- 推送到 develop 的 commit 数量；未推送工作树单独列出。
- 完整门禁结果：fmt、vet、build、test、race、e2e、交叉编译。
- 各平台包装状态，不把方案、交叉编译、可运行、可安装、签名、真机验收混成一个“完成”。
- 外部 agent 的最后报告时间、当前任务和等待原因。
- 当日缺口与具体补做命令。

每日 17 点值守自动化必须先更新 HTML 看板，再输出 KPI 汇报。HTML 是日常查看入口，本文是路线和完成判定的文字正典。
