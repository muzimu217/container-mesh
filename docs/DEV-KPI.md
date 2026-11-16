# container-mesh 开发与平台 KPI 看板

更新时间：2026-11-16 10:35Z。分支：`develop`。远端 tip：**`8c84e73`**。本轮新开发（**已在本地 worktree，尚未推送**）：

1. **P-3 bench frame 复用**：`internal/mesh/bench.go` 收发两侧改为复用单个帧缓冲。32MiB 双向累计分配 **699.34MB → 74.08MB（−89.4%）**；`readBenchFrame`/`sendBenchData` 已从分配榜消失，剩余热点全在 quic-go/socket 内核层。回环吞吐 1/8/32MiB 分别为 103–113 / 86–101 / 71–104 MiB/s，**P-3 目标 ≥200 MiB/s 仍未达标**（回环抖动大，需专用测量环境）。
2. **K-1 自动化覆盖**：`internal/cli/relay_stop_test.go` 补上 `cmesh relay` 优雅停止的进程内验收。**红→绿已证**：移除 `srv.Shutdown` 调用后测试红（"relay did not stop within 20s"），恢复后绿。`internal/cli/relay.go` 抽出 `serveRelay` 供测试驱动。
3. **F-1 接入 CI**：`packaging/install_test.sh` 从未进入 CI（这正是 F-1 连续逃逸两次的根因），已加入 `.github/workflows/ci.yml` 的端到端测试之后。本地 rc=0（"packaging install tests: ok"）。
4. **文档偏差修正**：`docs/architecture/MESH-DESIGN.md` §1.2 的"新建文件"表把 `internal/mesh/route.go`、`relay.go`、`agent.go`、`internal/e2e/e2e.go` 列为文件——**四个均不存在**。已加状态列并写明"当前 mesh 只有直连 QUIC，relay-only 报错 rc=1 不伪造"。
5. **仓库卫生**：`.gitignore` 补 `*.test` / `*.prof` / `*.out`（profiling 产物此前可被误提交）。
6. **测试稳定性**：`internal/mesh` 的握手超时 5s → 10s（与生产默认一致）。全量 `-race` 下曾出现一次 `TestPingRoundTrip` 的 QUIC 拨号超时，定位为 runner 负载下的 flaky，非产品缺陷。

push 前门禁全绿：fmt / vet / build / `go test ./... -race -count=1`（18 包）/ `tests/run-tests.sh --quick`（43/0 rc=0）/ `packaging/install_test.sh` rc=0 / 交叉编译 linux amd64+arm64、windows amd64、darwin amd64+arm64 全过。

外部复核：oh-no ✅（`8c84e73` panel / B-3 五动词+双向臂 / diagnostics / B-4 边界全项 PASS，无回退）；kuku 与 netns **连续两轮无回应**，本轮已升级为广播催办（Hub 四路 ok:true），仍未回报。prerelease `v1.0.0-rc1` 实际只有 **10 个资产**（矩阵需 20），`windows/arm64` 完全缺失——** darwin/windows 归档资产尚未上传完成**。Linux systemd 16/17 PASS（仅 reboot 待授权）。水位：`2026-11-16T09:47:24Z`。

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
| Linux headless 安装闭环 | **真机验收 16/17 PASS（仅 reboot 待授权）** | install.sh/cmesh-install：HTTPS+SHA 校验、版本目录/current symlink、卸载/保留状态/purge、mkrelease 加固均落盘；175 服务器 systemd 真机验收：启动/healthz/kill-9 自动拉起/SIGTERM 优雅停止/端口释放/升级回退/卸载全 PASS；F-1（离线回归 fixture 平台缺陷）已修复待推送 |
| 真实 Release / M4 安装验收 | **已在 v1.0.0-rc1 真资产 + 175 systemd VPS 验收（16/17，reboot 除外）** | prerelease `v1.0.0-rc1`（不占 latest 正式位）：安装器真实下载/SHA256 一致/版本化落盘 ✓；systemd 启动/healthz/kill-9 自动拉起(新 PID)/SIGTERM 优雅停止/端口释放 ✓；重装替换轮换+current 回滚 ✓；卸载保留状态 ✓。B 项篡改用例为脚本设计错误（篡改本地旧文件而非下载源），产品门禁由 A 项 SHA256 一致 + 离线回归覆盖，不构成缺陷。**未验收**：reboot 恢复（需重启生产服务器，待网主授权或另找可重启 VM）；darwin/windows 归档资产后台上传中 |
| systemd 前置与安全 | **示例未闭合，真实环境未验收** | unit 需 cmesh 用户/组、identity/account/members/account-pub 和非交互 secret；空口令可能 EOF，缺 account-pub 会降级证书验证；macOS 无法替代 systemd VPS/VM |
| 性能观测补全 | **本轮已补 direct bench 可重复口径，relay 侧仍缺** | 新增 `internal/mesh/bench_perf_test.go` 的 `BenchmarkRunBenchDirect`（1/8/32MiB 双向，报 MiB/s 与 allocs/op）；relay status 仍无 per-session RTT/吞吐，下一步只补测试观测，不先改协议 |
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
| 当前性能观测缺口 | **direct 已补，relay 仍待补** | direct `RunBench` 现已有可重复 bench 口径（MiB/s + allocs/op）；relay `/status` 仍只有累计消息/字节，没有 per-session RTT/吞吐。下一步补 relay 双向压测，再做前后对照；旧 p2p2 数据不冒充当前 QUIC |
| **F-1 discovery 热推送（1.05 MB/s）** | **已修复并经独立验证，红→绿已证** | 性能代理在真实 anchor 复现：任意一次性命令后，被用成员的 anchor 推送速率从 158 B/15s 暴涨到 **15.8 MB/15s（1.05 MB/s）且无界**（代理自己的 lab 测得 16,144,158 B/12s = **1,343,108 B/s**，放大 ×10,957）。根因：`Subscribe` 用 `close(old)` 废弃旧订阅，而**关闭的 channel 永远可读**，旧订阅者的 `case <-ch:` 立即且永久触发 → `streamMeshMembers` 陷入"推整份快照→立即再推"热循环。修复在**消费侧**：`case _, ok := <-ch` 检查 `ok`，`!ok` 即退出（旧流被新流取代，应静默退出）。红→绿：修复前测试红（"orphaned stream still receiving (757 bytes)"），修复后绿。**独立验证**：性能代理对落地后的 HEAD 做 A/B，**1,343,108 B/s → 151.1 B/s（−99.99%）**，4 次一次性命令后 226.8 B/s **不累积** |
| **P-3 直连吞吐 ≥200 MiB/s** | **未达标，已建立可重复基线** | 需求 `docs/REQUIREMENTS.md:194`。本轮新增 `BenchmarkRunBenchDirect`：回环 1MiB 103–113、8MiB 86–101、32MiB 71–104 MiB/s（双向合计）。bench frame 复用把 32MiB 累计分配从 699.34MB 降到 74.08MB（−89.4%），但**吞吐未变**——性能代理独立定位了原因：分配本身只占采样的 1.4%（`makeslice` 0.11s），真正的瓶颈是 **quic-go 的 per-packet syscall（`Syscall6` 18.18% flat）、加解密（9.48%）、memmove（8.83%）和内核→用户拷贝**（`readBenchFrame` cum 11.99%，其中 `io.ReadFull` 占 87.9%）。**可优化的项目代码只占 profile 的小头**，P-3 需要传输路径级改造，不是分配优化 |
| **K-1 relay 优雅停止自动化覆盖** | **本地完成，红→绿已证** | `internal/cli/relay_stop_test.go` 覆盖 `cmesh relay` 的 drain 语义（端口立即可再 bind）。**红→绿**：移除 `srv.Shutdown` 调用后测试红（"relay did not stop within 20s"），恢复后绿。`internal/cli/relay.go` 抽出 `serveRelay` 供测试驱动。外部进程级复核仍等 kuku |
| **F-1 接进 CI** | **本地完成** | `packaging/install_test.sh` 此前**从未**进入 CI——这是 F-1 连续逃逸两次的根因。已加入 `.github/workflows/ci.yml`。本地 rc=0 |
| **MESH-DESIGN 文档偏差** | **本地已修正** | §1.2"新建文件"表把 `internal/mesh/route.go`、`relay.go`、`agent.go`、`internal/e2e/e2e.go` 当作已有文件列出，四者**均不存在**。已加状态列并写明当前 mesh 只有直连 QUIC、relay-only 报错 rc=1 不伪造 |
| 优雅 LEAVE | **未开始，依赖连接生命周期接口** | 正常退出 ≤3s；kill -9 保持 ≤10s 兜底；迟到 LEAVE 不误杀重连成员 |
| 组网直连断点续传 | **未开始，独立协议任务** | 中断后 offset+hash 恢复；源文件变化、坏 `.part`、覆盖保护、空文件 |
| 远端 `@cat` 大输出 | **已在 `1f64cdc` 外部复测** | QUIC ACK reader 不再用 stream 级 `io.LimitReader(4097)`；kuku/oh-no 通过 4095/4096/4097B、64KiB、1MiB、5MiB 与 exit frame 校验 |
| 聚合遥测诚信 | **本地已修复，待下一推送** | 成员遥测缺失时全局速率/流量显示「未采集」；doubao 在 `1f64cdc` 复现 D-1，修复尚未进入远端 |
| 恢复可见性 flaky 测试 | **本地已修复，待下一推送** | 以 Joined/kill 前 register 基线/kill 后增量为事件断言；`-count=20` 全绿 |
| p2p2 快速重传 flaky 测试 | **本地已修复，待下一推送** | 移除 race/负载下不稳定的 `< RTO` 墙钟断言，保留 `FastRetrans > 0` 机制断言；20 次 race 单测全绿 |

## 性能研究结论与优化队列

- 已确认的瓶颈假设：relay 大负载 CPU 主要集中在 WebSocket 双向读写和 syscall；完整 mesh bench 的 `mesh.readBenchFrame`/`mesh.sendBenchData` 约占 alloc profile 主要部分；`p2pquic.readLoop` 每条消息分配新 buffer。
- **P1 第一项已完成（Round 49）**：bench frame buffer 复用已落地 `internal/mesh/bench.go`。32MiB 双向累计分配 **699.34MB → 74.08MB（−89.4%）**，`readBenchFrame`/`sendBenchData` 退出分配榜前 10；剩余 74MB 全部在 quic-go/socket 内核层（`socket.recvMsg`、`sockaddrToAddr`、`anyToSockaddr`），属传输库内部，不再由本仓代码贡献。**注意**：这只降低 GC 压力，**不改变吞吐**——P-3 的 ≥200 MiB/s 仍未达标。
- **分配降了但吞吐不动的原因（性能代理独立定位，F4）**：复用干掉的是 `makeslice`，它只占总采样的 **1.4%（0.11s）**——"分配降 89–94% 与吞吐不动"完全自洽。真正的瓶颈是传输路径：pprof top 显示 `Syscall6` **18.18% flat**、`gcmAesDec/Enc` **9.48%**、`memmove` **8.83%**、`futex` 4.74%；`readBenchFrame` cum **11.99%**（其中 `io.ReadFull` 87.9% = 内核→用户拷贝）。**可优化的项目代码只占 profile 的小头**——要提吞吐必须改传输路径（批量写、更少拷贝、更大流控窗口），不是继续压分配。
- **WAN 条件下吞吐崩塌（性能代理 F2）**：netns + netem 实测，0.1% 丢包 + 20ms 延迟 → 单向 16.9 → 5.6 MiB/s（−67%）；0.5% 丢包 → **1.2 MiB/s（−93%）**。机制是 quic-go 用 **Cubic**（`internal/ackhandler/sent_packet_handler.go:134`），`beta=0.7` 每次丢包把 cwnd 砍 30%。**16 个丢包 ≈ 吞吐掉 15 倍**。**加宽 QUIC 流控窗口经 A/B 排除、无收益**——所以这是拥塞控制行为而非配置疏漏，改进可能需要换算法（BBR）。这是"回环达标"不能代表公网达标的直接证据。
- **对旧结论的两处修正（性能代理独立复核）**：① bench frame 复用的**峰值 RSS 收益是 −14.3%（29,133 → 24,980 kB），不是此前记录的 −3.5%**——原结论偏保守；② 分配降幅实测 **−93.6%（342.9 → 21.8 MB）**，与主会话的 −89.4% 口径不同（是否含 TLS/QUIC 内部分配），**比例可比、绝对值不可互换**。
- **relay 是当前最重的组件（F5）**：64 MiB 端到端 **7.93–11.38 MiB/s**，relay CPU **27.0%–47.0% 单核**，峰值 RSS 16.0–17.3 MB；tx 放大系数 1.002×（dumb-pipe 成本：每字节读一次写一次）。
- **P-3 达标口径（F6）**：回环上界实测 **136.8–297.0 MiB/s**（50 MiB/方向，3 次，均值 227.7），但**方差极大（sd ≈ 80 MiB/s）**；netns 模拟 WAN 下 1.06–1.15 MiB/s。**P-3 在任何条件下都未稳定达标**——回环的 227.7 MiB/s 也不能算达标，因为离散度比阈值还大。
- **p2pquic 池化仍不可做（F7）**：`readLoop` 的 `make([]byte,n)` 无法直接池化——`wire.DecodeFrameBytes` 把 `Payload` **别名**到解密后的 buffer，`peer.byteQueue.Put` 再拷贝进环形队列，别名链上还有 `p.sec.OpenFrame(msg)` 持有原 slice。没有 ownership/Release 协议前池化会产生数据竞争/静默损坏。
- 优先级 P0：先实现 mesh relay trunk 与 direct/relay 路径选择；真实公网直连成功时可同时降低 relay CPU、RSS、双重拷贝和 RTT。
- 优先级 P1（剩余）：relay 双向压测补 per-session 吞吐/RTT 观测；**传输路径级优化**（批量写/减少拷贝/评估 BBR 替代 Cubic 以抗丢包）；每项必须补并发、关闭竞态、吞吐、GC/RSS 回归。
- 优先级 P2：对 relay WebSocket 32/64/128/512KiB frame 比较 `Send` 与流式写法；公网 WSS/443 和 Linux relay 复测前不下生产结论。
- 资源 profiling：分阶段采样空闲 join、discovery stream、direct QUIC、50/100MB bench、传输后回落，记录 `/proc/<pid>/status`、`smaps_rollup`、线程数、fd 数和 heap profile。
- 研究证据目录：`/tmp/perf-agent-round49.md`（本轮完整报告）、`/tmp/cmperf/`（脚本与 A/B 二进制）。本轮代理未修改仓库、未 commit/push/merge。

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

**本轮（Round 49）本地批次，全部通过**（2026-11-16 10:30Z，本地 worktree）：

- `gofmt -l internal/ cmd/ tools/`：无输出
- `go vet ./...`：PASS
- `go build ./...`：PASS
- `go test ./... -race -count=1`：PASS（18 包）
- `bash tests/run-tests.sh --quick`：PASS，`通过 43, 失败 0`，rc=0
- `sh packaging/install_test.sh`：PASS，rc=0（"packaging install tests: ok"）
- 交叉编译 PASS：linux/amd64、linux/arm64、windows/amd64、darwin/amd64、darwin/arm64
- 红→绿验证：`TestServeRelayGracefulStopReleasesPort` 在移除 `srv.Shutdown` 后红（"relay did not stop within 20s"），恢复后绿
- 分配对照：32MiB 双向 bench 累计分配 699.34MB → 74.08MB（−89.4%）
- 真实锚点面板：`/panel` 资源可达、令牌鉴权、假数据归零（oh-no 在 `8c84e73` 复核 PASS）

**已知一次性 flaky（本轮定位并缓解）**：全量 `-race` 下 `TestPingRoundTrip` 出现一次 QUIC 拨号超时（`context deadline exceeded`）。定位为 runner 负载下握手超过测试自设的 5s 截止时间，非产品缺陷；已把测试的 `HandshakeTimeout` 改为与生产一致的 10s，随后 `internal/mesh` 连跑 6 次 `-race` 全绿、全量 `-race` 再跑 3 次全绿。

## 并行开发队列

1. **M4 收口冲刺批次（2026-11-15）**：A=独立 CLI credential 黑盒 e2e ✅ 完成（quick 43/0，真实子进程链，未放宽 host policy）；B=mkrelease 加固 ✅ 完成（版本/平台矩阵校验、latest 输入拒绝、LICENSE 与 cwd 解耦、归档可列举校验、显式 --license，gofmt/vet/test 全绿）；C=serve-discovery SIGTERM 优雅停止 ✅ 完成；D=授权 mclaw 真实 Linux 安装/systemd 验收 ✅ 完成（16/17，仅 reboot 待授权）。本轮补 E=relay SIGTERM 自动化覆盖 ✅、F=install_test 接入 CI ✅。批次结果只记命令/退出码/数字，不把方案写成完成。
2. **LEAVE + 撤销切断**：共享 discovery/mesh 连接事件接口，先设计后并行实现，避免两个代理改同一路径。
3. **组网断点续传**：独立 loopback QUIC 故障注入，使用 `.part` + offset + hash，避免与 LEAVE 任务冲突。
4. **平台宿主**：先 Linux headless 安装闭环；再桌面最小 Wails 宿主；之后 Android/iOS 最小 gomobile binding + 真机单链路。
5. **性能研究**：P1 bench frame 复用已落地（分配 −89.4%）；下一步 relay 双向压测观测、P-3 吞吐达标口径（分离握手与传输时间）、`p2pquic` buffer pool 需先定 ownership/Release 生命周期。

## 外部等待

- `doubao-cloud-vm-01`：最新 `2026-11-16T09:18:19Z`，F-1 在 linux/amd64 复测通过（rc=0）；已回信 `ok:true`。
- `oh-no-agent`：最新 `2026-11-16T09:47:24Z`，`8c84e73` 上 panel / B-3 五动词+双向臂 / diagnostics / B-4 边界**全项 PASS、无回退**；已回信 `ok:true`。
- `kuku-sandbox-01`：最新 `2026-11-16T02:54:19Z`（`6f07562`）。此后静默已超 6 小时，且上一轮针对 `8c84e73` 的具体催办**无回应**；本轮已**升级为广播催办**（三项唯一指定任务：relay SIGTERM 必须 exit 0 / quick 43-0 且 rc 契约 / serve-discovery 优雅退出），Hub 返回 `{"ok":true}`。
- `netns-probe`：最新 `2026-11-16T02:13:52Z`（`6f07562`）。同上，本轮已**升级为广播催办**（NAT 方向性复测 / RSS 基线 20 点 / 50MB bench 后回落），Hub 返回 `{"ok":true}`。
- **darwin/windows 归档资产**：`gh release view v1.0.0-rc1` 实测只有 **10 个资产**，平台矩阵（linux/darwin/windows × amd64/arm64）需 20 个；缺 `cmesh-v1.0.0-rc1-darwin-amd64`、`cmesh-latest-darwin-{amd64,arm64}`、`cmesh-latest-linux-arm64`、`cmesh-v1.0.0-rc1-windows-amd64` 及其 tar.gz，且 **`windows/arm64` 完全不存在**。Windows 命名在三处不一致（release 无 `.exe` / installer 要 `.exe` / mkrelease 出 `.exe`），需重建发布而非只补上传。
- 网主：**①** 交互会话放行本轮 worktree 变更的提交与 develop 推送；**②** reboot 恢复验收需要重启生产服务器 175.24.139.148 的授权，或提供一台可重启的 disposable VM（会中断该机上的锚点/hub/nginx）。
