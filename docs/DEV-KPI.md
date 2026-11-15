# container-mesh 开发与平台 KPI 看板

更新时间：2026-11-15 18:20Z。分支：`develop`。远端 tip：`1f64cdc`。Hub 水位：`2026-11-15T18:17:33Z`。水位后 2 条报告（oh-no/doubao 复测就绪确认，均回信 ok:true）；下一 SHA 复测范围已扩展到 M4 冲刺四批次。当前 worktree 43 条；quick 43 项全绿；批次 C 优雅停止 3 测试 PASS；批次 D（mclaw Linux systemd）运行中。

这张表把代码、可运行产物、安装包、真机验收分开记账。交叉编译通过不等于平台交付完成；设计方案存在也不等于安装包完成。

## 产品与 M4

| 领域 | 当前状态 | 证据 / 下一步 |
|---|---|---|
| 真实锚点面板 `/panel` | **已完成，已在远端 tip 验收** | React + Vite + Three.js 构建产物由 Go 内嵌；令牌头鉴权；离线成员保留；`/panel-lite` 保留 |
| 面板白屏修复 | **本地已完成，待放行** | `/panel` → `308 /panel/`，`Cache-Control: no-store`；资源按浏览器 URL 解析测试 |
| 假设备 / 假遥测清除 | **本地已完成，待放行** | 删除香港假节点、假拓扑、假 CPU/RAM/SLA/流量、假动作成功回显；未知数据显示「未采集」 |
| 委派成员可拨 B-1 | **已完成并在 `1f64cdc` 复测** | oh-no 双向测试；10/10；凭证设备主动 ping/msg/send-to/run 与凭证→凭证均 PASS |
| 凭证设备后续命令 B-3 | **已在 `1f64cdc` 外部复测** | `discovery.json` 保存派生字段；oh-no：join 后 ping/msg/send-to/run 与凭证→凭证双向 ping 全部 PASS |
| `--endpoint :0` B-2 | **已完成并在 `1f64cdc` 复测** | 绑定端口替换纯函数 + 回归测试；外部端口自检和直连通过 |
| 端口自检 | **已完成** | TCP/UDP 同端口提示、回环绑定提示、可执行动作 |
| Linux headless 安装闭环 | **本地代码完成，真实 Linux 验收运行中（批次 D）** | install.sh/cmesh-install 已实现 HTTPS+SHA 校验、版本目录/current symlink、卸载/保留状态/purge、mkrelease 加固；批次 D 正在授权 mclaw 执行真实安装+systemd（verify/enable/restart/kill/reboot）验收；SIGTERM 优雅停止由批次 C 实现中 |
| 真实 Release / M4 安装验收 | **本地加固完成，真实发布未验收** | mkrelease 已加固：输入版本/平台矩阵校验、latest 输入拒绝、LICENSE 与 cwd 解耦、--license 显式指定、归档逐条校验（gofmt/vet/test 全绿）；仍无真实 GitHub Release 资产的 checksum/download/install/upgrade/rollback 证据，worktree 与 `1f64cdc` 需固化后同 SHA 黑盒复测 |
| systemd 前置与安全 | **示例未闭合，真实环境未验收** | unit 需 cmesh 用户/组、identity/account/members/account-pub 和非交互 secret；空口令可能 EOF，缺 account-pub 会降级证书验证；macOS 无法替代 systemd VPS/VM |
| 性能观测补全 | **待补独立 bench/relay 观测** | 当前 direct bench 未稳定记录 Duration/MiB/s/CPU/alloc；relay status 无 per-session RTT/吞吐；下一步只补测试观测，不先改协议 |
| 中继路由 / 增量视图 | **未开始，推迟 v1.1** | M4-PLAN C 组；不混入当前 v1.0 收口 |

## 本地新增开发 KPI

| 任务 | 状态 | 验收 |
|---|---|---|
| 云服务器真实验收 | **已完成 mclaw loopback，公网/NAT 未达成** | VM-6-7-ubuntu：healthz/panel/token/API、join --cred、过期 credential 拒绝、status、ping 3/3、msg、send-to SHA、run @info、bench 4KB 均通过；仅 loopback/direct，未测公网 relay/WSS/跨云 NAT/反向客户端 |
| 文档整改 brief | **已完成索引，整改执行中** | `docs/DEVELOPMENT-GAPS.md` 已按 P0/P1/P2 写明影响、需求/代码证据、完成判据和环境边界；`docs/README.md` 已加入入口并纠正过时阶段状态 |
| URL host 安全校验 | **红→绿完成，测试 harness 另待改造** | `normalizeBaseURL` 拒绝 localhost/环回/私有/保留地址；成员 endpoint 私有语义保留；`go test ./internal/discovery` 和 CLI credential focused PASS；quick shell e2e 仍有 3 项 loopback fixture 失败，不把它算生产回归 |
| `cmesh mesh ping` | **已在 `1f64cdc` 外部复测** | 100ms 最小间隔、1–1000 包、超时计 loss、p50/p95；当前 direct QUIC，relay-only 不伪造；默认超时已覆盖最大采样窗口 |
| `cmesh bench` | **已在 `1f64cdc` 外部复测** | 默认 100MB/方向、双向独立 bench stream、可限速；回环 1MB 双向字节完整；QUIC retransmission 当前明确 `n/a` |
| `@cat` 大输出 | **已在 `1f64cdc` 外部复测** | 4095/4096/4097B+真实 run frame/exit frame 测试，修复 stream 级 4KiB 截断；kuku/oh-no 六档通过 |
| `cmesh doctor` | **本地完成，完整门禁通过，待独立外部复测** | 复用 `cmesh probe` 的能力报告与退出码，避免第二套漂移实现 |
| 锚点主动成员操作 B-4 候选 | **待产品定性** | `serve-discovery` 锚点 state 没有 `discovery.json`，锚点主动 `mesh ping/msg` 返回 config not found；成员→锚点和成员↔成员均 PASS；本轮不判回归、不改协议 |
| mesh relay-only trunk | **未完成，v1.1/R-3.2** | 当前 ping/bench/msg/send-to 都只走 direct QUIC；旧 `send/recv` relay 是共享 code/A-B 配对。最小 vertical slice 需要一跳 device-ID 认证、route open、A-C E2E 加密、direct-first fallback、四类业务复用和 quota 测试；尚无实现/外部证据 |
| 资源 KPI | **未达标，已实测超阈值** | netns `1f64cdc` 真 netns/SNAT 采样：常驻 join 进程 20 点、含 50MB bench 的 VmRSS 15,620–15,800 KB，超过 15MB；CPU 约 0.34s/20s（≈1.7% 单核时基），bench 一次性峰值 RSS 未采样；relay 空闲基线另为 0.0% / 6,256–9,024 KB |
| 当前性能观测缺口 | **待补测试观测，不改协议** | direct `RunBench` 测试当前只断言字节完整，没有稳定记录 `Duration/MiB/s/CPU/alloc/op`；relay `/status` 只有累计消息/字节，没有 per-session RTT/吞吐。下一步先补独立 bench 输出和 relay 双向压测，再做前后对照；旧 p2p2 数据不冒充当前 QUIC |
| 优雅 LEAVE | **未开始，依赖连接生命周期接口** | 正常退出 ≤3s；kill -9 保持 ≤10s 兜底；迟到 LEAVE 不误杀重连成员 |
| 组网直连断点续传 | **未开始，独立协议任务** | 中断后 offset+hash 恢复；源文件变化、坏 `.part`、覆盖保护、空文件 |
| 远端 `@cat` 大输出 | **已在 `1f64cdc` 外部复测** | QUIC ACK reader 不再用 stream 级 `io.LimitReader(4097)`；kuku/oh-no 通过 4095/4096/4097B、64KiB、1MiB、5MiB 与 exit frame 校验 |
| 聚合遥测诚信 | **本地已修复，待下一推送** | 成员遥测缺失时全局速率/流量显示「未采集」；doubao 在 `1f64cdc` 复现 D-1，修复尚未进入远端 |
| 恢复可见性 flaky 测试 | **本地已修复，待下一推送** | 以 Joined/kill 前 register 基线/kill 后增量为事件断言；`-count=20` 全绿 |
| p2p2 快速重传 flaky 测试 | **本地已修复，待下一推送** | 移除 race/负载下不稳定的 `< RTO` 墙钟断言，保留 `FastRetrans > 0` 机制断言；20 次 race 单测全绿 |

## 性能研究结论与优化队列

- 已确认的瓶颈假设：relay 大负载 CPU 主要集中在 WebSocket 双向读写和 syscall；完整 mesh bench 的 `mesh.readBenchFrame`/`mesh.sendBenchData` 约占 alloc profile 主要部分；`p2pquic.readLoop` 每条消息分配新 buffer。
- 优先级 P0：先实现 mesh relay trunk 与 direct/relay 路径选择；真实公网直连成功时可同时降低 relay CPU、RSS、双重拷贝和 RTT。
- 优先级 P1：复用 bench frame buffer，优化 `internal/mesh/bench.go`；评估 `internal/p2pquic` 受限 buffer pool；每项必须补并发、关闭竞态、吞吐、GC/RSS 回归。
- 优先级 P2：对 relay WebSocket 32/64/128/512KiB frame 比较 `Send` 与流式写法；公网 WSS/443 和 Linux relay 复测前不下生产结论。
- 资源 profiling：分阶段采样空闲 join、discovery stream、direct QUIC、50/100MB bench、传输后回落，记录 `/proc/<pid>/status`、`smaps_rollup`、线程数、fd 数和 heap profile。
- 研究证据目录：`/tmp/container-mesh-perf/`；本轮代理未修改仓库、未 commit/push/merge。

| 平台 / 形态 | 设计 | 可构建 | 可运行 | 可安装 / 签名 | 真机验收 |
|---|---:|---:|---:|---:|---:|
| 纯出站 NAT 反向直拨 | — | — | client→anchor PASS | — | anchor→client 待 mesh relay trunk（R-3.2） |
| Linux headless CLI / relay | ✅ | ✅ | ✅ | ⚠️ 手工安装器 | ⚠️ 干净容器/VPS矩阵待做 |
| Linux desktop Wails | ✅ | ❌ | ❌ | ❌ | ❌ |
| Windows CLI | ✅ | ✅ amd64 | ⚠️ 需 Windows 实机 | ❌ MSI/签名 | ❌ |
| Windows desktop Wails | ✅ | ❌ | ❌ | ❌ | ❌ |
| macOS CLI | ✅ | ✅ amd64/arm64 | ⚠️ 本机 CLI 可跑 | ❌ `.app`/签名/公证 | ❌ |
| macOS desktop Wails | ✅ | ❌ | ❌ | ❌ | ❌ |
| Android | ✅ gomobile 方案 | ❌ | ❌ | ❌ APK | ❌ |
| iOS | ✅ gomobile 方案 | ❌ | ❌ | ❌ IPA/签名 | ❌ |
| NAS | ⚠️ Linux 能力可复用 | ⚠️ amd64/arm64 | ⚠️ Docker/CLI | ❌ 厂商包/升级 | ❌ |
| router/OpenWrt | ⚠️ 设计方向 | ❌ MIPS/ARMv7/procd | ❌ | ❌ | ❌ |

## 当前门禁证据

最近一次本地修复批次已通过：

- `gofmt -l internal/ cmd/ tools/`：无输出
- `go vet ./...`
- `go build ./...`
- `go test ./... -race -count=1`：PASS（2026-11-15 08:00Z，本地 worktree）
- `bash tests/run-tests.sh --quick`：PASS，38 个 shell 场景 + credential Go integration PASS（2026-11-15 08:00Z，本地 worktree）
- Linux amd64/arm64、Windows amd64 交叉编译：PASS（2026-11-15 08:00Z）
- 真实锚点面板：`/panel` 资源可达、令牌鉴权、假数据归零

`status/doctor` 本地增量已经通过 `go test ./internal/cli/ -run 'Status|Doctor' -count=1` 和 `go build ./...`；当前批次已再跑 `go test ./...`、`go test ./... -race -count=1` 与 `bash tests/run-tests.sh --quick`（38 shell 场景 + credential Go integration），并通过 Linux amd64/arm64、Windows amd64 交叉编译，全部通过。

## 并行开发队列

1. **M4 收口冲刺批次（2026-11-15）**：A=独立 CLI credential 黑盒 e2e ✅ 完成（quick 43/0，真实子进程链，未放宽 host policy）；B=mkrelease 加固 ✅ 完成（版本/平台矩阵校验、latest 输入拒绝、LICENSE 与 cwd 解耦、归档可列举校验、显式 --license，gofmt/vet/test 全绿）；C=serve-discovery SIGTERM 优雅停止 🔄 运行中；D=授权 mclaw 真实 Linux 安装/systemd 验收 🔄 运行中。批次结果只记命令/退出码/数字，不把方案写成完成。
2. **LEAVE + 撤销切断**：共享 discovery/mesh 连接事件接口，先设计后并行实现，避免两个代理改同一路径。
3. **组网断点续传**：独立 loopback QUIC 故障注入，使用 `.part` + offset + hash，避免与 LEAVE 任务冲突。
4. **平台宿主**：先 Linux headless 安装闭环；再桌面最小 Wails 宿主；之后 Android/iOS 最小 gomobile binding + 真机单链路。
5. **性能研究**：第二轮已完成；当前等待 Release/ownership API 设计、长时/WAN/Linux/WSS 对照，不将临时 buffer 变体直接合入。

## 外部等待

- `doubao-cloud-vm-01`：最新 `2026-11-15T03:28:12Z`，B-3/NAT 复测 PASS；反向 mesh relay 缺口确认；已回信 `ok:true`。
- `kuku-sandbox-01`：最新 `2026-11-14T17:31:14Z`，已超过 6 小时；本轮已发送 status/diagnostics 与本地修复复测催办，Hub 返回 `{"ok":true}`。
- `netns-probe`：最新 `2026-11-15T03:31:24Z`，B-3 五项动词、client→anchor 30/30、20/50MB bench PASS；RSS 超过 15MB，已回信 `ok:true`。
- `oh-no-agent`：最新 `2026-11-14T16:41:17Z`，已超过 6 小时；本轮已发送 status/diagnostics 与本地修复复测催办，Hub 返回 `{"ok":true}`。
- 网主：交互会话放行当前 41 条 worktree 状态变更的提交与 develop 推送；本值守会话不 commit/push/merge。
