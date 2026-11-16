# container-mesh 项目总规划与交付口径

更新时间：2026-11-16。当前分支：`develop`。远端 tip：`9cc946e`。本轮（Round 49–56）共推送 **14 笔**、9 次推送，远端 CI 每轮均 success。**①** P-3 bench frame 复用（32MiB 双向累计分配 699.34MB → 74.08MB，−89.4%，吞吐未变；oh-no 三轮字节完整性专项全过）；**②** **F-1 discovery 热推送缺陷修复**（`close(old)` 使旧订阅 channel 永远可读 → 旧流热循环，1,343,108 B/s → **151.1 B/s，−99.99%**，**四方独立验证闭环**）；**③** **出口地址策略补齐三条路径**（原只拒绝字面私网 IP，**解析后地址无人复核** → 主机名可穿透 = SSRF；现 members WS / register HTTP / enroll HTTP **三条全受约束**，经 `Dialer.Control` 在 DNS 后连接前判定；逃生口用 build tag `cmeshtest` 隔离，`go tool nm` 确认**出货二进制无此路径**）；**④** **relay 前向循环读缓冲未复用**（从未采用 `RecvInto` 扩容后的缓冲 → 每条 >64KiB 消息重付 64→128→256→512KiB 分配拷贝链；实测消息平均 493KiB 属常态；`makeslicecopy` 占 relay CPU 23.73% → 4.88%，**CPU 中位 −71%**）；**⑤** **多端点 fallback 整体失效修复**（复用同一 UDP socket + quic-go single-use Transport）；**⑥** `cmesh relay` 优雅停止自动化覆盖；**⑦** `packaging/install_test.sh` 接入 CI；**⑧** **systemd unit 五处缺口修复**（含缺 `--account-pub` → **入网端点根本不挂载**）；**⑨** README 安装命令 404 修复；**⑩** mkrelease 归档目录名带 `.exe` 修复；**⑪** 快照路径补 `MaxEndpoints` 上限（恶意快照 500 端点全收）；**⑫** **调用方 deadline 不约束端点循环**（`status --probe --probe-timeout 3s` 实测跑 5.12s）；**⑬** **join 保存的端点仍是 `:0`**（announce 与配置文件读的是两个不同的列表 → 成员短暂离线）；**⑭** **PR-1「路径选择的骨架」已落地**（3 笔，**不含 relay 能力**：错误分类 sentinel 覆盖 quic-go「Dial 返回 nil、错误到 read ack 才浮现」+ Session 抽象 11 处签名重写 + PathPicker（`Picker==nil` 直接走原路径，无第二条代码路径）+ RelayCandidates 纯排序；施工代理另修「多端点只保留最后一个错误 → ErrAuth 被 ErrUnreachable 抹掉」）。**性能结论两次被推翻并更正**：quic-go 暴露的是**每连接**丢包计数（度量前提**已打开**）、拥塞控制是 **Reno 不是 Cubic**（数值巧合导致名错，换算法需 fork quic-go）；**P-3 崩塌机制是 cwnd 塌缩**；**Linux 604–638 MiB/s 达标、darwin 233–270 不达标**（GSO 仅 Linux）。**资源 KPI 从「未达标」改记「达标」**：本地 150 秒实测稳态 **7.7–9.7 MB**（<15MB 目标），此前记录的 75,268 KB 是 **pre-GC 相位**。**oh-no 报的「凭证成员 kill -9 检出 70–90s」经值守实测复核为非回归**（HEAD 0.1s / 历史 SHA 0.2s；根因是判据用错——anchor 默认日志级别 Warn，快速离线路径不打日志）。

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
- e2e 已纳入第 11 节；当前 worktree quick 门禁为 38 个 shell 场景 PASS + credential Go integration PASS。

远端 tip `6fc00c6` 已包含 status/ping/bench、@cat 修复、聚合遥测诚信修复、两项 flaky 测试修复、relay 优雅停止（Shutdown 生命周期）、e2e 凭证段真实 CLI 化、安装器离线回归与 systemd unit 资产。status/ping/bench/@cat 此前已在 `1f64cdc` 通过 oh-no/doubao/kuku 外部复测；`6fc00c6` 的新增面（relay 优雅停止、e2e 43、install 回归、面板 308）待同 SHA 外部复测。

最新外部边界补充：oh-no 在 `1f64cdc` 发现 B-4 候选——`serve-discovery` 锚点 state 没有 `discovery.json`，锚点主动 `mesh ping/msg` 无发起路径；成员→锚点、成员↔成员均 PASS。本轮按状态边界记录，等待产品确认锚点是否必须支持主动成员操作，不以此为回归直接改协议。

性能第二轮最终结果：64KiB bench frame 临时复用在真实 mesh QUIC 64MiB 双向中将累计分配从 328.11MB 降到 54.38MB（-83.4%），峰值 RSS 从 20,768KB 降到 20,032KB（-3.5%），CPU profile 没有稳定收益；relay 64/128MiB 平均 CPU 9.27%/20.34%、峰值 RSS 17,792/17,968KB，仍超过目标。`p2pquic.readLoop` 2MiB profile 分配约 2,086KiB，不能直接池化，必须先定义消息 slice 所有权和 Release 生命周期。该轮为临时变体，未进入生产代码。

**性能第三轮（Round 49，本地 worktree）**：P1 第一项 bench frame buffer 复用已**正式落地** `internal/mesh/bench.go`（收发两侧各复用一个帧缓冲）。32MiB 双向累计分配 **699.34MB → 74.08MB（−89.4%）**，`readBenchFrame`/`sendBenchData` 退出分配榜前 10，剩余 74MB 全部在 quic-go/socket 内核层。**吞吐没有变化**：回环 1MiB 103–113、8MiB 86–101、32MiB 71–104 MiB/s，P-3 的 ≥200 MiB/s 仍未达标——瓶颈不在分配而在传输路径本身。新增 `BenchmarkRunBenchDirect` 作为可重复口径，但当前每次迭代新建 listener、握手时间混入，离散度大，**不能作为达标证据**；下一步需分离握手与传输时间再判。

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

M4 硬阻塞复核（2026-11-15 更新）：真实 Release 与 Linux systemd 验收已在 prerelease `v1.0.0-rc1` + 175 服务器完成（16/17）：安装器真实下载+SHA256 一致、systemd 启动/kill-9 自动拉起/SIGTERM 优雅停止、重装轮换回滚、卸载保留状态。**剩余两项**：① reboot 恢复（需重启生产服务器或另找可重启 VM，待网主授权）；② 候选 SHA `6f07562` 的外部 agent 黑盒复测（已派发，水位居 18:17:33Z）。M4 完成判定四条中第 1/2/4 条已达成，第 3 条（同 SHA 复测）进行中。

### M4-D：二进制、安装器与 README

- Go 交叉编译和 `tools/mkrelease` 组装器已完成。
- `latest` 别名、归档和 `SHA256SUMS` 逻辑已完成。
- `packaging/install.sh` 和 `tools/cmesh-install` 已入本地工作树。
- README 命令级错误已修正：口令旗标、参数顺序、凭证路径、relay 参数和发布件数字。
- Linux headless 代理审计确认：当前 `packaging/install.sh` 与 `tools/cmesh-install` 只下载/校验/写单一二进制，未实现 systemd unit、专用服务用户、journald、卸载保留状态、原子升级/回滚、SIGTERM 优雅停止和重启恢复；`mkrelease` 也未纳入 systemd/卸载资产，且文档声称的 installer release asset 不一定生成。下一步需在真实 Debian/Ubuntu systemd VPS/VM 完成安装、restart、kill 自动拉起、升级失败回滚、reboot 恢复和 send/recv smoke；当前 macOS 只能做脚本/构建检查，不能标 Linux 安装完成。

### 本地新增运维线

- `cmesh status`：读取已验证 snapshot，输出成员、版本、在线状态、端点、`last_seen` 和保守 reachability；新增显式 `--probe <deviceID>` 直连 mTLS/QUIC 探针，成功写 `direct`，失败写 `degraded` + `probe_error`，无探针仍为 `unknown`。
- `cmesh mesh ping`：100ms 最小采样间隔、最多 1000 包、丢包统计、p50/p95，direct/degraded/unknown 不伪造；当前只支持 direct QUIC。
- `cmesh bench`：默认每方向 100MB，独立 direct QUIC 测试流，支持 rate 限制，不写业务文件；QUIC 重传计数不可观测时明确输出 `n/a`；当前没有 mesh relay bench。
- `cmesh doctor`：复用现有 probe 能力与退出码。
- `@cat` 大输出：修复 ACK reader 的 4 KiB stream 截断，新增长输出 QUIC 回归测试。

## 当前未完成

### 性能研究与优化线

性能研究与功能交付并行推进，不以“等待 relay 实现”为停工理由。每轮由独立代理执行可重复实验，覆盖 direct QUIC、discovery WebSocket、阶段 1 relay send/recv 和未来 mesh relay trunk；记录拓扑、命令、样本数、CPU/RSS、吞吐、RTT、丢包、重连、内存分配、瓶颈假设和优化建议。研究结论按本地上界、模拟 NAT、真实公网分层；访问现有公网服务必须使用授权的 http/https 路径，拒绝本机/私有/保留地址，凭据只读环境变量或密钥服务。性能数字没有实验命令和样本就不进入 KPI。

当前首轮研究结果已归档：macOS M4 loopback 上 direct QUIC 2MiB 单向 137.7–235.2MiB/s；完整 mesh bench 3MiB 双向 10/10、约 75–100MiB/s；阶段 1 relay 在 16/64/128MiB 下平均 CPU 4.75%/9.37%/17.36%、峰值 CPU 8.8%/27.6%/46.8%，峰值 RSS 16,240/17,216/17,264KB。结论是 relay 大负载超过资源目标，mesh bench frame 分配和 `p2pquic.readLoop` buffer 分配是优先 profiling 点；这些是 macOS loopback 结果，不是公网结论。

下一轮性能实验：1) bench frame buffer 复用；2) `p2pquic.readLoop` 受限 buffer pool；3) relay 32/64/128/512KiB 写路径对比；4) Linux/WSS/443/真实 NAT 与公网授权环境复测；5) 分阶段 `/proc` + `smaps_rollup` 峰值 RSS 采样。每项优化必须有前后对照数字和回归测试。

### M4-C / v1.1：中继路由与增量视图

尚未开始。当前阶段 1 `send/recv` relay 使用共享 code + A/B 配对，只能证明配对文件/消息通道，不能作为 mesh device-ID relay。mesh relay trunk 需要独立的成员认证、目标路由、端到端加密、配额和路径回退协议；在该协议和无 UDP endpoint 的独立测试完成前，`mesh ping/bench/msg/file` 不宣称 relay-only。

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
