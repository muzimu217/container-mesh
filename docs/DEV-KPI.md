# container-mesh 开发与平台 KPI 看板

更新时间：2026-11-16 12:20Z。分支：`develop`。远端 tip：**`47cff18`**。本轮共推送 7 笔提交、3 次推送（`7a66f5e` → `d206c14` → `47cff18`），每次推送前完整门禁全绿，远端 CI 三轮均 **success**（含新加入的「离线安装回归」CI 步骤并通过）。

**已推送的 7 笔**：`7e4bd2c` perf(mesh) bench frame 复用 / `2739355` test(cli) relay 优雅停止覆盖 + 握手 flaky / `f81a2f5` ci 离线安装回归 + gitignore / `7a66f5e` docs 文档偏差修正 / `a4080e8` fix(relay) **F-1 热推送** / `d206c14` docs 性能发现 / `47cff18` docs 独立验证记录。

**① P-3 bench frame 复用**（`7e4bd2c`）：`internal/mesh/bench.go` 收发两侧改为复用单个帧缓冲。32MiB 双向累计分配 **699.34MB → 74.08MB（−89.4%）**；`readBenchFrame`(288MB/41%) 与 `sendBenchData`(277MB/39%) 已从分配榜消失，剩余热点全在 quic-go/socket 内核层。**吞吐未变**——回环双向 1/8/32MiB 分别 103–113 / 86–101 / 71–104 MiB/s，**P-3 目标 ≥200 MiB/s 仍未达标**。独立验证：oh-no 在 `7a66f5e` 做字节完整性专项，双向 sent/received 全部精确相等、对抗尺寸（4095/4097/65535/65537/1048577/33554431）无损、同进程连跑 3 轮 32MiB 无跨帧残留、单测 `-race ×3` 全绿。

**② F-1 discovery 热推送（1.05 MB/s）**（`a4080e8`）：性能代理在真实 anchor 复现——任意一次性命令后，被用成员的 anchor 推送速率从 158 B/15s 暴涨到 **15.8 MB/15s**，代理自测 lab 测得 16,144,158 B/12s = **1,343,108 B/s**（放大 ×10,957），无界。根因：`Subscribe` 用 `close(old)` 废弃旧订阅，**关闭的 channel 永远可读**，旧订阅者的 `case <-ch:` 立即且永久触发 → `streamMeshMembers` 陷入"推整份快照→立即再推"热循环。修复在**消费侧**（`case _, ok := <-ch`，`!ok` 即退出），不动 `Subscribe` 语义。红→绿：修复前红（"orphaned stream still receiving (757 bytes)"），修复后绿；复核时手动置空 `if !ok` 重跑**再次确认红**。**独立验证**：代理对落地后的 HEAD 做 A/B，**1,343,108 B/s → 151.1 B/s（−99.99%）**，4 次一次性命令后 226.8 B/s 不累积。

**③ relay 优雅停止自动化覆盖**（`2739355`）：`cmesh relay` 此前**零进程级测试**（K-1 修复没有自动化保护）。`internal/cli/relay.go` 抽出 `serveRelay` 供测试驱动。**红→绿已证**：移除 `srv.Shutdown` 调用 → 测试红（"relay did not stop within 20s"）；恢复 → 绿。

**④ F-1 接进 CI**（`f81a2f5`）：`packaging/install_test.sh` **从未**进入 CI——这正是 F-1 连续逃逸两次的根因（"CI 绿"对安装器毫无意义）。已加入 `.github/workflows/ci.yml`，远端 CI 该步骤已通过。

**⑤ 文档偏差修正**（`7a66f5e`）：`docs/architecture/MESH-DESIGN.md` §1.2 的"新建文件"表把 `internal/mesh/route.go`、`relay.go`、`agent.go`、`internal/e2e/e2e.go` 列为文件——**四个均不存在**。已加状态列并写明"当前 mesh 只有直连 QUIC，relay-only 报错 rc=1 不伪造"。

**⑥ 仓库卫生 + flaky 缓解**（`f81a2f5` / `2739355`）：`.gitignore` 补 `*.test`/`*.prof`/`*.out`；mesh 测试握手超时 5s → 10s（与生产默认一致），全量 `-race` 下曾出现一次 `TestPingRoundTrip` 的 QUIC 拨号超时，定位为 runner 负载下 flaky、非产品缺陷。

**外部复核**：oh-no ✅ 三轮全项 PASS（`8c84e73` / `407fde3` / `7a66f5e` 字节完整性专项）；doubao ✅（F-1 在 linux/amd64 rc=0）；kuku 与 netns **连续两轮无回应**，本轮已升级为广播催办并再次派单（Hub 各路 ok:true），仍未回报。prerelease `v1.0.0-rc1` 实际只有 **10 个资产**（矩阵需 20），`windows/arm64` 完全缺失。Linux systemd 16/17 PASS（仅 reboot 待授权）。水位：`2026-11-16T12:05:24Z`。

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
| **真实 Release / M4 安装验收** | **安装器闭环已验证，但发布资产严重不全** | prerelease `v1.0.0-rc1`：**实际资产 10 个**（`gh release view`），矩阵需 20。逐 URL 实测：`cmesh-v1.0.0-rc1-{linux-amd64,linux-arm64,darwin-arm64}` **206 可用**；`darwin-amd64` **404**；`windows-amd64`（两种命名）**404**；`cmesh-latest-{linux-arm64,darwin-amd64,darwin-arm64,windows-amd64}` 全 **404**；`windows/arm64` 连 SHA256SUMS 都没有。**SHA256SUMS 列了 15 行，实际资产 10 个——7 行悬空**，`sha256sum -c --ignore-missing` 恰好跳过它们，让「校验通过」变成假象。**安装器实测**：`CMESH_VERSION=v1.0.0-rc1` → rc=0，版本化落盘 `versions/v1.0.0-rc1/` + `current` symlink + `.cmesh-install-root` 齐全，`--uninstall` rc=0；默认 `VERSION=latest` → **rc=1 404**。<br>**更正上一轮「资产来源不可追溯」——不成立**：`go version -m` 显示已发布二进制**内嵌 VCS 信息**（`vcs.revision=6f07562d37e31358b7ff18dbeecb1f02936f8dc1`、`vcs.time=2026-11-15T19:53:33Z`、`vcs.modified=true`），审计代理用「干净 clone + `-trimpath` + 复现 `+dirty`」配方**字节级复现 5/5 digest 精确命中**。真正的问题是**仓库里没把这个写下来**，而不是来源混乱。<br>**另更正「已发布 rc1 不含 K-1」**：它**含** serve-discovery 的 K-1，**不含** `relay` 的 K-1（`18404f1` 晚 12.6 小时）——此前用 `strings` 查 `relay: shutdown` 命中 0 是因为查的是 relay 路径而非 serve-discovery。<br>**未验收**：reboot 恢复（需网主授权或可重启 VM）；缺失资产重建上传（需网主放行发布动作） |
| systemd 前置与安全 | **示例已存在但有 5 处实质缺口，未在真 systemd 上验收** | **更正上一轮的「仓库内无 systemd unit」——`packaging/systemd/` 存在**（`cmesh-discovery.service` + `cmesh-discovery.env.example` + README）。但实测缺口：① `ExecStart` **无 `--account-pub`** → 缺该参数时发现节点**根本不挂载入网端点**（凭证入网不可用），且证书验证降级为「certs 目录即信任」；② 无 `StateDirectory=`（`ReadWritePaths=/var/lib/cmesh` 依赖目录已存在）；③ 无 `TimeoutStopSec=`（K-1 的 drain 有 10s 上限，unit 未与之对齐）；④ `EnvironmentFile=-` 前缀 `-` 表示**文件缺失不报错**，于是 `${CMESH_STATE}` 等展开为空 → `--state ""` 静默回落 `~/.cmesh`（对 `User=cmesh` 是家目录，且与 `ProtectHome=read-only` 冲突）；⑤ 空口令在无 tty 时 EOF。**验收状态**：真 systemd 255 容器（PID 1）已验证启动 exit 0 / SIGTERM 优雅停 exit 0（非 143）/ 端口真释放 / kill -9 拉起新 PID（由 M4 审计代理实测）——但**真机 Linux + 服务用户 + 日志落盘仍未验收** |
| 性能观测补全 | **本轮已补 direct bench 可重复口径，relay 侧仍缺** | 新增 `internal/mesh/bench_perf_test.go` 的 `BenchmarkRunBenchDirect`（1/8/32MiB 双向，报 MiB/s 与 allocs/op）；relay status 仍无 per-session RTT/吞吐，下一步只补测试观测，不先改协议 |
| 中继路由 / 增量视图 | **未开始，推迟 v1.1** | M4-PLAN C 组；不混入当前 v1.0 收口 |

## 本地新增开发 KPI

| 任务 | 状态 | 验收 |
|---|---|---|
| 云服务器真实验收 | **已完成 mclaw loopback，公网/NAT 未达成** | VM-6-7-ubuntu：healthz/panel/token/API、join --cred、过期 credential 拒绝、status、ping 3/3、msg、send-to SHA、run @info、bench 4KB 均通过；仅 loopback/direct，未测公网 relay/WSS/跨云 NAT/反向客户端 |
| 文档整改 brief | **已完成索引，整改执行中** | `docs/DEVELOPMENT-GAPS.md` 已按 P0/P1/P2 写明影响、需求/代码证据、完成判据和环境边界；`docs/README.md` 已加入入口并纠正过时阶段状态 |
| **URL host 安全校验（出口策略缺口）** | **本地已修复（两条路径），红→绿已证** | 上一轮只实现了策略的**一半**：`allowedDiscoveryHost` 拒绝 loopback/私有/保留**字面 IP**，其注释把剩下的交给拨号方——「callers that dial them must use an egress policy that revalidates resolved addresses before connecting」——但**从未实现**：WebSocket 拨号用的是裸 `net.Dialer`，**解析后的地址无人复核**。因此一个解析到回环/私网的主机名可直连（kuku 在沙箱实测 `disc.local → 127.0.0.1` 穿透成功）。发现地址是运维提供的值 → 这是 **SSRF 形态**。修复：`wsconn.DialOptions` 增加可选 `EgressPolicy`，经 `net.Dialer.Control` 在 **DNS 解析后、socket 连接前**回调；discovery 客户端**始终**传入。stage-1 relay 保持原行为（其端点本就常是私网地址，**有意不启用**）。e2e 套件需要连 127.0.0.1 → 用 **build tag `cmeshtest`** 把逃生口编译进去；**`go build`、CI 构建、发布构建都不带该 tag**，故出货二进制没有逃生口，并有测试断言之（`go tool nm` 亦确认 `internal/cmeshtest` 未链接进生产二进制）。红→绿：去掉策略后接线测试红（"the client dialled without an egress policy"），恢复后绿。<br>**★ 补第二半（doubao 独立实测发现）**：策略最初只覆盖了 **members WS 流**，**register 的 HTTP POST 走的是裸 `http.Client`、无策略** → 同一个 origin 的两条路径一条被拒一条可达，**SSRF 面只关了一半**。doubao 用 127.0.0.1 listener 实测：members 流被正确拒绝（`egress policy: ... resolved to 127.0.0.1, which is not a public address`），而 register 请求**真实到达**（listener 回 `501 Unsupported method`）。已修：`client.go` 的 `httpTransport()` 给 register 加上同一策略（同样在 dialer 内按解析后地址判定，连接根本不会建立）。红→绿：新增测试用**裸 listener**（`httptest.Server` 会在测试看到连接前就拒掉不认识的方法 → **假绿**）且把 origin 排除出本地代理（否则代理替 origin 应答、测试根本看不到连接），修复前红（"the register request reached a loopback address"），修复后绿 |
| `cmesh mesh ping` | **已在 `1f64cdc` 外部复测** | 100ms 最小间隔、1–1000 包、超时计 loss、p50/p95；当前 direct QUIC，relay-only 不伪造；默认超时已覆盖最大采样窗口 |
| `cmesh bench` | **已在 `1f64cdc` 外部复测** | 默认 100MB/方向、双向独立 bench stream、可限速；回环 1MB 双向字节完整；QUIC retransmission 当前明确 `n/a` |
| `@cat` 大输出 | **已在 `1f64cdc` 外部复测** | 4095/4096/4097B+真实 run frame/exit frame 测试，修复 stream 级 4KiB 截断；kuku/oh-no 六档通过 |
| `cmesh doctor` | **本地完成，完整门禁通过，待独立外部复测** | 复用 `cmesh probe` 的能力报告与退出码，避免第二套漂移实现 |
| 锚点主动成员操作 B-4 候选 | **待产品定性** | `serve-discovery` 锚点 state 没有 `discovery.json`，锚点主动 `mesh ping/msg` 返回 config not found；成员→锚点和成员↔成员均 PASS；本轮不判回归、不改协议 |
| mesh relay-only trunk | **未完成，v1.1/R-3.2** | 当前 ping/bench/msg/send-to 都只走 direct QUIC；旧 `send/recv` relay 是共享 code/A-B 配对。最小 vertical slice 需要一跳 device-ID 认证、route open、A-C E2E 加密、direct-first fallback、四类业务复用和 quota 测试；尚无实现/外部证据 |
| 资源 KPI | **口径已厘清；稳态 7.7–9.7 MB 达标，但采样窗口决定读数** | **本轮值守本地实测（macOS，`ps -o rss`，150 秒窗口）**：常驻 join 的 RSS 序列为 25,968 → 25,952 → **7,712 → 7,888 → …** 稳定在 **7.7–9.7 MB**，末值 9,744 KB —— **低于 15 MB 目标，达标**。GODEBUG=gctrace 佐证：gc 1 在 0.002 s（64 MB goal），gc 2 在 **120.282 s** 把 `65->65->0 MB`，即前 15 秒的 RSS 峰值是**启动churn + 运行时尚未把空闲内存还给 OS**（Go 后台 scavenger 是惰性的），不是稳态。**netns 报的 75,268 KB 全平是「pre-GC 相位」**（上轮 `6f07562` 的 16,360 KB 是「post-GC 相位」，两者不可直接比）。**结论：资源 KPI 从「未达标」改记「达标（按 post-GC 稳态口径）」，但必须写明采样窗口**——前 15 秒内的读数（25–26 MB）会误判为超标。bench 期间的峰值 RSS（历史上 ~139 MB）本轮未复现（netns 测得 75,268 KB 全平、零增长），与 bench frame 复用方向一致，但 netns 明确声明不做因果归因，本会话亦不归因 |
| 当前性能观测缺口 | **direct 已补，relay 仍待补** | direct `RunBench` 现已有可重复 bench 口径（MiB/s + allocs/op）；relay `/status` 仍只有累计消息/字节，没有 per-session RTT/吞吐。下一步补 relay 双向压测，再做前后对照；旧 p2p2 数据不冒充当前 QUIC |
| **多端点 fallback 整体失效** | **本地已修复，红→绿已证** | `internal/mesh/direct.go` 的 `DialDirect` 在端点循环里**复用同一个 UDP socket**，而 `dialOne` 在失败时 `udp.Close()`；quic-go 的 `quic.Dial` 把 `net.PacketConn` 包进 **single-use Transport**（`quic-go@v0.55.0 transport.go: isSingleUse`），会话结束即关连接 → **第一次尝试失败后，后续端点全在已关闭的 socket 上拨号**，报 `use of closed network connection`。后果：成员宣布多个端点（私网 + NAT 映射）时，**fallback 永远走不到活端点**——这也是「直连失败→回退中继」的前置阻塞。修复：每次尝试单独 bind 一个 socket。**红→绿**：修复前测试红（`all 2 endpoint(s) ... failed: use of closed network connection`），修复后绿且能 ping 通。**由 mesh 中继干道设计代理发现** |
| **F-1 discovery 热推送（1.05 MB/s）** | **已修复并经三方独立验证，红→绿已证** | 性能代理在真实 anchor 复现：任意一次性命令后，被用成员的 anchor 推送速率从 158 B/15s 暴涨到 **15.8 MB/15s（1.05 MB/s）且无界**（代理自己的 lab 测得 16,144,158 B/12s = **1,343,108 B/s**，放大 ×10,957）。根因：`Subscribe` 用 `close(old)` 废弃旧订阅，而**关闭的 channel 永远可读**，旧订阅者的 `case <-ch:` 立即且永久触发 → `streamMeshMembers` 陷入"推整份快照→立即再推"热循环。修复在**消费侧**：`case _, ok := <-ch` 检查 `ok`，`!ok` 即退出（旧流被新流取代，应静默退出）。红→绿：修复前测试红（"orphaned stream still receiving (757 bytes)"），修复后绿。**三方独立验证**：① 本会话红→绿 757 bytes ② 性能代理自建 lab **1,343,108 B/s → 151.1 B/s（−99.99%）** ③ doubao 真实拓扑 30 次命令后 **0.1–0.5 KB/s**（差 3 个数量级）+ 独立红→绿 769 bytes；oh-no 另测缺陷版 **8 次命令累计 645,532 B 且 15s 窗口持续 MB 级不收敛**，修复版 **8 次仅 12 B** |
| **P-3 直连吞吐 ≥200 MiB/s** | **未达标，已建立可重复基线** | 需求 `docs/REQUIREMENTS.md:194`。本轮新增 `BenchmarkRunBenchDirect`：回环 1MiB 103–113、8MiB 86–101、32MiB 71–104 MiB/s（双向合计）。bench frame 复用把 32MiB 累计分配从 699.34MB 降到 74.08MB（−89.4%），但**吞吐未变**——性能代理独立定位了原因：分配本身只占采样的 1.4%（`makeslice` 0.11s），真正的瓶颈是 **quic-go 的 per-packet syscall（`Syscall6` 18.18% flat）、加解密（9.48%）、memmove（8.83%）和内核→用户拷贝**（`readBenchFrame` cum 11.99%，其中 `io.ReadFull` 占 87.9%）。**可优化的项目代码只占 profile 的小头**，P-3 需要传输路径级改造，不是分配优化 |
| **K-1 relay 优雅停止自动化覆盖** | **本地完成，红→绿已证** | `internal/cli/relay_stop_test.go` 覆盖 `cmesh relay` 的 drain 语义（端口立即可再 bind）。**红→绿**：移除 `srv.Shutdown` 调用后测试红（"relay did not stop within 20s"），恢复后绿。`internal/cli/relay.go` 抽出 `serveRelay` 供测试驱动。外部进程级复核仍等 kuku |
| **F-1 接进 CI** | **本地完成** | `packaging/install_test.sh` 此前**从未**进入 CI——这是 F-1 连续逃逸两次的根因。已加入 `.github/workflows/ci.yml`。本地 rc=0 |
| **出口策略全路径覆盖** | **审计完成，第三条路径已补齐；第四条待实测** | 派了只读审计代理**枚举全仓出站连接路径**，结论：真正缺策略的只有 **2 条**，其余 **6 条有意不修**（附理由）。**M-1 已修**（`44b9a8f`）：**enroll 的 HTTP POST**（`internal/cli/join_cred.go`）用裸 `&http.Client{}` → 同一个 discovery origin 的**第三条路径**（前两条是 register POST 与 members WS 流）。字面检查有（`NormalizeDiscoveryURL`），**解析后检查缺**。已改为 `discovery.EgressTransport()`，与 register/members 共用同一实现；`enrollWithCredential` 拆出可注入 transport 的变体（原测试靠替换全局 `http.DefaultTransport` 指向进程内 server——现在改为注入，那个全局替换本身也是隐患，已删除）。红→绿：去掉策略后测试红（"the enrollment request reached a loopback address"），恢复后绿。<br>**M-2 待实测（不凭代码阅读下结论）**：**mesh direct QUIC 拨号**（`internal/mesh/direct.go:737`）的端点来自**远端快照**，而 `direct.go:677-681` 只拒 wildcard `0.0.0.0`、**不拒 loopback**；discovery 节点的信任模型明写 "trusted to be online, **not to be honest**"（`client.go:602`），它可以重写快照端点 → 恶意节点可让客户端把 QUIC Initial 发到 `127.0.0.1:任意端口`（内网端口扫描 + 拨号挂起，16 端点 × 10s = 160s）。**但成员端点不能套 egress policy**——私网成员地址是**合法且核心**的产品形态（套上去会砸掉产品）。审计代理判断应套一个**语义不同**的策略：**拒 loopback/保留段，放行私网**。**已派实测代理**把"机制是否真实存在"从代码推理变成实测数字（裸 UDP 监听器计数，注意本项目刚踩过的坑：断言必须是"对端没收到包"而非"客户端没报错"），**实测出来之前不改代码**。<br>**有意不修的 6 条**（附理由）：stage-1 relay（`internal/cli/node.go:309`，运维自己指定 + 私网合法 + SPAKE2 已防 MITM）、stage-1 打洞（已配对对端，无新增能力）、self-announce（目标是本机自己）、probe（诊断用途）、cmesh-install（独立工具）、forward 拨号（**已有更强策略**：allow-list + 钉住 IP 防 rebinding，套 egress 反而降低安全性）。 |
| **mesh relay trunk** | **设计已产出，施工计划已就绪，尚未施工** | 设计裁定「中转方 = **网内成员节点**」（R-3.2「不是专用中继」胜出；决定性依据是**锚点必须本身是成员**，所以现网里专用 relay 就是成员进程内的一个角色）。裁定为成员即中转方后，**E2E 加密从可选变成必须**（中转方在信任模型内，密码学上必须封住它）→ 需 SIGMA（`crypto/ecdh` + `crypto/hkdf`，**Go 1.24+ 已在 go.mod 内，无新依赖**，已在 /tmp 独立验证可编译）。<br>**施工计划已产出**（`/tmp/trunk-plan-round52.md`）：7 项前置件逐项给了改动清单、依赖、工时、风险、红→绿测法，并划成**两个可独立验证的 PR**：**PR-1 = 路径选择的骨架**（①错误分类 sentinel + ②PathPicker + ③' RelayCandidates + ⑤-1 Session 抽象，**不含 relay 能力**，只把业务层从 `*quic.Stream` 解耦 → PR-2 只需"加一条路"而非"改所有签名"）；**PR-2 = 中转干道**（④e2e SIGMA + ⑤-2 干道协议 + ⑤-3 配额 + ⑤-4 push-open 限定 ping/msg + ⑥CLI 接线）。<br>**规划代理复核后更正了设计稿的 7 处**：**I-1** 设备 ID 路由表/`FrameMeshRoute` **v1.1 不做**（1 跳中转无环路场景；需求 `REQUIREMENTS.md:233` 明写择优选路归 R-3.3）；**I-2** 「直接复用 `forward` 的 tokenBucket」**不成立**（它**未导出**且单位是 **streams/sec**，不是 P-7 要的 50 MiB/s 字节配额 → v1.1 在 mesh 内自写字节桶 ~50 行）；**I-3** 加第二个 ALPN 会**改变现网所有 direct 连接的协商结果**（必须显式断言 direct 仍是 `container-mesh-direct/1`）；**I-4** 业务函数有 **9 个 `*quic.Stream` 硬编码签名**，不先抽 Session 接口 relay 接不进去（设计稿漏算这 8h）；**I-5** 同账号检查需新增接口（`MemberResolver` 只有 `KeyFor`，无账号维度）→ v1.1 用「同一张成员视图」近似；**I-6** mesh 现有帧是**自有 4 字节长度前缀**、与 `internal/wire` 的 10 字节头**完全不同** → 建议干道控制消息用 wire 帧、业务载荷继续用 mesh 帧（wire 无内部依赖 → 不成环）；**I-7** push-open **不能砍**（需求要求）但**限定 ping/msg 级别**，文件传输推到 v1.2。 |
| **MESH-DESIGN 文档偏差** | **本地已修正** | §1.2"新建文件"表把 `internal/mesh/route.go`、`relay.go`、`agent.go`、`internal/e2e/e2e.go` 当作已有文件列出，四者**均不存在**。已加状态列并写明当前 mesh 只有直连 QUIC、relay-only 报错 rc=1 不伪造 |
| **PR-2 待网主/主代理裁决的两点** | **未决，PR-2 开工前必须落定** | **G11**：push-open 的 **B 侧连接生命周期**未裁定（设计稿 G4）——在「B 主动连 C」与「C 主动连 B」之间做设计决策，属**设计工作不是规划工作** → **PR-2 开工前**先写一条 `docs/decisions.md`（T-0.4）。**G14**：PR-2 是否该拆成 3 个 PR（把风险最高的 ⑤-4 push-open 单独出）→ 取决于对 PR 数量的偏好。**PR-1 不受这两点阻塞**（它不含 relay 能力，已可独立开工） |
| **relay 前向循环的读缓冲未复用** | **本地已修复，红→绿已证** | `internal/relay/relay.go` 的前向循环把一个读缓冲穿过 `RecvInto`。消息放不下时 `RecvInto` 会在**自己内部**扩容并返回扩容后的切片，但循环读的是**自己的** `buf = buf[:cap(buf)]`——**从未看到那次扩容** → 每条大于初始 64KiB 的消息都把 64→128→256→512 KiB 的分配+拷贝链**重付一遍**。实测 relay 的真实消息**平均 493 KiB**（远超初始缓冲），所以这是常态而非边角；profile 里 `makeslicecopy` 占 relay CPU **23.73%**，修复后降到 4.88%，ABAB 各 9 次测得 relay CPU 中位 **0.310s → 0.090s（−71%）**、吞吐中位 +11%、最坏 +231%。修复：循环改用读实际返回的缓冲（`buf = msg[:cap(msg)]`）。红→绿：新增 relay 侧缓冲生长计数，修复前测试红（"grew its read buffer 20 times for 20 messages"），修复后绿 |
| **README 安装命令 404** | **本地已修复（`1712099`），待放行** | README §1 原本让用户从 `releases/latest/download/...` 下载——GitHub 只对**非 prerelease** 的正式 release 提供该路径，而仓库唯一 release 是 prerelease `v1.0.0-rc1`，实测 **HTTP 404**。已改为版本化地址 `releases/download/v1.0.0-rc1/...`，并**端到端实测**：下载 rc=0 → `sha256sum -c SHA256SUMS --ignore-missing` 输出 `cmesh-v1.0.0-rc1-linux-amd64: OK` → install 落盘成功。同时补明：darwin/amd64 与 windows 资产**尚未上传**、Windows 带 `.exe` 后缀、`--ignore-missing` 会掩盖「清单承诺但文件不存在」 |
| **发布资产矩阵缺失** | **需网主放行（本地无法代劳）** | 缺件精确清单（逐 URL 实测）：`cmesh-v1.0.0-rc1-darwin-amd64`、`cmesh-v1.0.0-rc1-windows-amd64.exe`、`cmesh-v1.0.0-rc1-windows-arm64.exe`、`cmesh-latest-linux-arm64`、`cmesh-latest-darwin-amd64`、`cmesh-latest-darwin-arm64`、`cmesh-latest-windows-amd64.exe`（+ 对应 tar.gz）。**命名约定以 `.exe` 为准**（`tools/mkrelease/main.go:283-288` 与 `packaging/install.sh:90` 都按 `.exe` 工作），是这次发布没按约定上传。重建发布时须让 SHA256SUMS 与实际资产一致——现在的 7 行悬空来自上传环节手工拼装，不是 mkrelease |
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
- **WAN 条件下吞吐崩塌（性能代理 F2）**：netns + netem 实测，0.1% 丢包 + 20ms 延迟 → 单向 16.9 → 5.6 MiB/s（−67%）；0.5% 丢包 → **1.2 MiB/s（−93%）**。**机制更正（性能代理本轮实证）**：quic-go v0.55.0 用的是 **Reno 不是 Cubic**（`internal/ackhandler/sent_packet_handler.go:134-140` 的 `true, // use Reno`，上游 v0.55.0 与 v0.65.0 双向核对）。Reno 的 `renoBeta=0.7` 与 Cubic 的 `beta=0.7` **数值巧合**，所以「砍 30%」的数字碰巧对，但**算法名错、「换 Cubic」这条路线根本不存在**。**换拥塞控制不可行**：quic.Config 无任何 congestion 字段，编译直接报 `use of internal package .../internal/congestion not allowed`，上游最新 v0.65.0 仍硬编码 Reno → **唯一路径是 fork quic-go**。
- **P-3 崩塌机制的直接测量**：新增 cwnd 追踪显示 netem 20ms/0% 丢包时 `cwnd_max` = 5.6 MB，1% 丢包时掉到 111–186 KB（**−30~50×**），吞吐 39.6→1.7 MiB/s。同一档 rep=3 的 111 个丢包含 **40 个 spurious**，cwnd 反而保住 1.98 MB → **证明是 cwnd 塌缩而非丢包数决定吞吐**。
- **「减少 syscall」本仓可控空间 ≈ 0**：Linux `Syscall6` 23.59% flat，逐层 `-peek` 全部在 quic-go 内部（`Transport.listen`→`oobConn.ReadPacket`；`sendQueue.Run`→`sconn.writePacket`→`oobConn.WritePacket`）。本仓代码仅 9.30% cum，其中 86% 是 `io.ReadFull` 内核→用户拷贝。**补充：darwin vs Linux 差 2.3–3.4×，根因是 GSO 仅 Linux** —— Linux **604–638 MiB/s（P-3 达标）**，darwin 233–270（不达标）。
- **每流重传计数可以拿到（上一轮的「阻塞项」是错的）**：quic-go 暴露的是**每连接**计数，两条独立路径都能拿到——`Conn.ConnectionStats().PacketsLost`（`quic-go/connection.go:800-813`）+ `logging.ConnectionTracer.LostPacket`（`logging/connection_tracer.go:28`）。已在 /tmp 副本实现 `BenchStats.Retransmissions` 从 nil 变真实值并新增 `BenchLoss`（本地）+ `PeerLoss`（对端）双向上报，两条独立计数器实测一致（98/98、369/369、175/175、207/208，差异为 spurious）。**度量前提已打开**。
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

- `oh-no-agent`：最新 `2026-11-16T14:31:10Z`。**四轮全项 PASS 无回退**（`8c84e73` / `407fde3` / `3ca269e` bench 字节完整性专项 + F-1 外部 A/B / `3ca269e` `mesh ping --count 1000` 上限复核）。F-1 A/B：缺陷版 8 次命令累计 **645,532 B** 且 15s 窗口持续 MB 级**不收敛**，修复版 **12 B**。
- `netns-probe`：最新 `2026-11-16T12:52:42Z`（第五十五轮）。NAT 方向性：入网 **0.565 s**、ping **30/30**、p50 0.395ms / p95 0.537ms；反向锚点→客户端**第四次同形仍不可达**（R-3.2 边界、不判缺陷）。RSS 报「20 点全平 75,268 KB」——**本会话已厘清为 pre-GC 相位**，稳态 7.7–9.7 MB 达标。
- `kuku-sandbox-01`：最新 `2026-11-16T13:50:31Z`（静默 9.9h 后**终于回报**）。**relay SIGTERM exit 0 三轮全过**（43–46ms、端口 0.0–0.1ms 内可重绑）、**serve-discovery SIGTERM exit 0 五轮全过**、**e2e 退出码契约 PASS**（绿 rc=0 / 刻意失败 rc=1，失败项原文一致）。如实申报：**e2e 原样执行 rc=1（`通过 42, 失败 1`）**，失败项是 `mkfifo` 被沙箱 seccomp 拦截所致，代理用进程替换适配后得 43/0 —— 属**环境限制**、非产品缺陷。另有 **6 条观察项**，其中一条是**安全观察**（主机名穿透出口策略）→ **本会话已修复**。
- `doubao-cloud-vm-01`：最新 `2026-11-16T16:24:39Z`。**①** `packaging/install_test.sh` 在 linux/amd64 独立复跑 **rc=0**，CI 接线仍在。**②** 出口策略 linux 实测（三例）：**A.** join 指向解析到回环的名字（`127.0.0.1.nip.io`）→ **被拒**（rc=1，`egress policy: 127.0.0.1:39711 resolved to 127.0.0.1, which is not a public address`）；**B.** 指向解析到私网的名字（`10.0.0.1.nip.io`）→ **被拒**；**C.** 正向对照，指向真实公网 IP → **照常工作**（`mesh ping` 5/5、p50 0.312ms、link=direct），**公网路径无回归**。**D.** 逃生口隔离实测：`go tool nm | grep -c cmeshtest = 0`（生产无逃生口）、带 tag 构建时断言按设计红、默认构建绿。<br>**★ 观察项（本会话已修）**：register **HTTP 路径**未被 egress 策略覆盖——members WS 流被拦，但 `announce`/register 的 HTTP POST 走裸 `http.Client`，实测真实连通（listener 回 `HTTP 501 Unsupported method`）→ **SSRF 面只关了一半**。已由本会话补上 `httpTransport()`，红→绿已证。<br>环境事实：该 VM 公网 IP 已迁移 `9.106.227.17 → 9.100.127.194`（旧地址 bind 失败暴露）。
- **darwin/windows 归档资产**：`gh release view v1.0.0-rc1` 实测只有 **10 个资产**（矩阵需 20 个）。逐 URL 实测缺件：`cmesh-v1.0.0-rc1-darwin-amd64`、`cmesh-v1.0.0-rc1-windows-amd64.exe`、`cmesh-v1.0.0-rc1-windows-arm64.exe`、`cmesh-latest-linux-arm64`、`cmesh-latest-darwin-amd64`、`cmesh-latest-darwin-arm64`、`cmesh-latest-windows-amd64.exe`（+ 对应 tar.gz）；`windows/arm64` 连 SHA256SUMS 都没有。**SHA256SUMS 列 15 行、实际资产 10 个，7 行悬空**。**命名约定以 `.exe` 为准**（`tools/mkrelease/main.go:283-284`、`packaging/install.sh:89-90,96`、`tools/cmesh-install/main.go:68-71` 三处源码**已全部一致**）→ **错的只有已发布资产**，因此修复是**0 行代码改动**、需重建并重发。**未上传资产本会话不能代劳**（发布动作需网主放行）。
- 网主：**①** 交互会话放行本轮 worktree 变更的提交与 develop 推送；**②** reboot 恢复验收需要重启生产服务器 175.24.139.148 的授权，或提供一台可重启的 disposable VM（会中断该机上的锚点/hub/nginx）；**③** 是否授权重建并上传缺失的 darwin/windows 归档资产（并让 SHA256SUMS 与实际资产一致）。
