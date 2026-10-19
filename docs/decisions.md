# 技术选型：两台 NAT 后低权限容器之间的互通

> 版本信息核对时间：2026-10-17。croc v11.5.4、Syncthing v2.1.6、iroh v1.3.0、pion/webrtc v4.2.24。

## 0. 约束（来自主代理实测，直接采信）

| 事实 | 影响 |
|---|---|
| 容器 root，`CapEff=a00c05fb` | 无 `CAP_NET_ADMIN`(12)、`CAP_NET_RAW`(13)、`CAP_SYS_ADMIN`(21)、`CAP_MKNOD`(27) |
| `/dev/net/tun` 不存在 | **TUN 完全不可用**，任何基于 TUN 的组网方案出局 |
| TCP + UDP 出站均可用，无入站 | 只能做「主动外连 + 中继/打洞」 |
| croc 在容器内可用，公共 relay 可达 | 证明「出站 TCP/WebSocket 到公共 relay」这条路走得通 |
| EasyTier `--no-tun` 在容器内异常（连 127.0.0.1 都 2 秒超时，本地 macOS 正常） | **不基于 EasyTier 设计**；这个现象本身也是重要线索，见 §5.2 |
| 上海出口经透明代理，`github.com → 198.18.0.5`，TCP 握手 0.1ms | 典型 fake-IP 代理特征，见 §5.1 |
| Cloudflare Tunnel 100MB 限制只作用于 HTTP 请求体 | WebSocket 流式传输不受限，本项目采用 WS 是对的 |

## 1. 四个方案的真实工作量与可行性

先说结论：**推荐 croc 作为近期可用方案，本项目自研的 WebSocket 中继方案作为可控替代**。不推荐 iroh（Go 侧不可用）、不推荐 pion 自建（工作量与运维成本远超收益）、不推荐 Syncthing（面向目录同步而非一次性传输，内存与协议开销不划算）。

### 1.1 pion/webrtc 自建

**要自己写的东西**：信令服务（WebSocket 服务器 + 房间管理）、ICE 服务器配置、TURN 服务器（coturn）部署与运维、DataChannel 上的分块协议、断点续传状态机、背压控制、文件元数据交换。

- 小消息：DataChannel 直接给消息语义，工作量中等。
- 大文件：**必须自己实现分块 + 流控**。DataChannel 有 `bufferedAmount` 但需要自己做窗口，否则内存爆掉。断点续传也要自己设计（记录 offset、校验 prefix）。
- 打洞：依赖 ICE，能力等价于浏览器 WebRTC。
- TURN 运维：coturn 需要公网 IP、开放 3478/udp+tcp 与中继端口段（默认 49152-65535），还要处理证书、认证、配额。

**真实工作量估计**：可用原型 3-5 人日；能上生产（TURN 高可用、监控、限速、NAT 类型诊断）3-6 人周。

**关键问题：两台都在 NAT 后时打洞成功率？**
公开数据（Chrome UMA 统计口径）大致是：直连成功 75-80%，20-25% 需要 relay。分 NAT 类型看，full-cone ≈98%、address-restricted ≈90%、port-restricted ≈75%、**symmetric ≈20%**。两个都在 symmetric NAT 后（企业网、部分移动网络、以及本例这种「透明代理出口」）基本必然走 relay。

**判断**：这个容器环境是「透明代理出口 + 上海机房」，不是典型家宽。代理层往往会破坏 UDP 打洞（fake-IP 场景下 STUN 返回的映射地址可能根本不可用），实际成功率应该按**偏差情形**估计，即大概率走 relay。所以「自建 WebRTC」的真实形态是「自建 TURN」，那不如直接用一个简单的 WS 中继。

**TURN 传 GB 文件的带宽成本**：
- Twilio：US/EU $0.40/GB，亚太 $0.60/GB，悉尼/圣保罗 $0.80/GB。
- 注意计费口径：TURN 按**双向**计费，中继 1GB 文件实际计费约 2GB。所以 100GB 文件走亚太 TURN ≈ 200GB × $0.60 = **$120**。
- 自建 coturn：Hetzner 约 €5-15/月含 20TB 流量；阿里云/腾讯云按流量约 ¥0.5-0.8/GB，100GB 双向 ≈ ¥100-160。

### 1.2 iroh（QUIC + 内建 relay + 打洞）

**优点**：设计最现代。QUIC + TLS1.3，按公钥拨号，QNT（QUIC-NAT-Traversal）把打洞做成 QUIC 层操作，relay 是无状态的（转发加密包，不维护会话），运维成本理论上最低。官方称打洞成功率约 90%，95% 的数据量走直连。

**致命问题（对本项目）**：**Go 绑定不可用于生产**。
- 官方 `iroh-ffi` 提供 Python/Swift/Kotlin/Node.js 绑定，Go 明确标注 "Community maintained"。
- Go 绑定是 `git.coopcloud.tech/decentral1se/iroh-go`，需要 cgo + 预编译 Rust 动态库 + `LD_LIBRARY_PATH`，支持矩阵只有 **Linux x86_64/aarch64 (musl)**，且是社区维护的 FFI 包装。
- 本项目要交付「真实可运行且本地验证」的代码，本机是 macOS arm64，Go 绑定在这里跑不起来。用 Rust 重写则与现有工具链（Go 1.27 已就绪，Rust 未安装）冲突。

**判断**：iroh 在架构上是最优解，但**当前 Go 生态落地成本过高**。如果将来团队愿意用 Rust 或 Python，iroh 值得重估。注意其「90% 打洞成功」是项目自述，独立分析也提醒应视为文档声明而非基准测试结果。

**relay 成本**：iroh relay 无状态，带宽仍是主要成本，量级与 TURN 相同（数据必须过 relay）。自建 `iroh-relay` 的成本 ≈ 一台 VPS + 出网流量。

### 1.3 croc（已验证可用）

**优点**：
- 容器内**实测可用**（主代理已验证，能拿到 transfer code，公共 relay 可达）。
- 断点续传、目录传输、PAKE 加密、压缩，全部现成。
- 中继是纯 TCP，`croc relay` 一条命令自建，端口 9009-9013。

**关键认知纠正**：croc 的 WAN 模式**不做 UDP 打洞**，它就是「双方各自出站连 relay，relay 把两条 TCP 连接 staple 起来」。所以：
- 打洞成功率这个问题对 croc **不适用**——它 100% 走 relay（除非同 LAN，此时用本地 relay 走直连）。
- 换句话说，croc 的可靠性等于「relay 可达性」，这在本环境里已经被验证。
- 代价是**所有流量都过 relay**，没有直连优化。传 GB 文件时 relay 带宽就是成本。

**relay 带宽成本**：作者自述公共 relay 2023 年每月 20TB 流量、$40-50/月（早期 $5/月）。这个数字说明：单条 relay 的带宽成本并不高，但如果自建且流量大，按云厂商流量计费仍然可观。自建 `croc relay` 在 Hetzner 这类含大流量的 VPS 上最划算。

**工作量**：几乎为零（已有二进制）。缺点是它是「文件传输工具」，不是「双向消息通道」——`croc send` 传文本也算，但要做长连接双向聊天、自定义协议，就得嵌它的库或者干脆自己写。

### 1.4 Syncthing（用户态、P2P+中继、断点、增量）

**优点**：用户态、无 TUN、断点续传、块级增量、加密、成熟。

**缺点（针对本任务）**：
1. **它是目录同步，不是一次性传输**。要传一个文件得建共享文件夹、配对设备 ID、等扫描。语义不匹配。
2. **协议重**：Block Exchange Protocol，需要索引、块哈希数据库。单条协议消息上限 500MB（间接限制单文件约 1.25GB，实际现代版本已改善但仍有历史包袱）。
3. **内存与 CPU**：社区反馈 relay 每会话约 4MB 缓冲，1000 会话需 4GB；客户端本身也不轻。低权限容器里跑常驻同步守护进程不划算。
4. **中继带宽**：社区 relay 普遍 <10Mbit，实测中继传输约 540MB/min vs 直连 928MB/min。自建 `strelaysrv` 可以解决速度，但**默认会加入公共 relay pool 帮别人转发流量**，必须显式 `-pools=""` 才是私有。
5. 打洞依赖 UPnP/NAT-PMP + 中继，成功率与 WebRTC 同量级。

**判断**：除非需求变成「两台机器持续镜像某个目录」，否则 Syncthing 是过度设计。

## 2. 直接回答两个关键问题

### Q1: 两台都在 NAT 后时，打洞成功率有多高？

| 场景 | 估计成功率 | 依据 |
|---|---|---|
| 双方家宽 port-restricted cone | 75-90% | 公开 NAT 类型统计 |
| 双方 symmetric NAT | ~20% | 同上 |
| 企业网/代理出口 | 20-50% | 防火墙策略不可控 |
| **本例（上海 + 透明代理）** | **大概率失败，按走 relay 设计** | 代理层改写/劫持 UDP，fake-IP 使 STUN 映射不可信 |

结论：**不要把打洞当作前提**。任何方案都必须有 relay 兜底，并且要能「先用 relay 建立，再尝试升级到直连」（iroh/Tailscale 的做法），而不是「先打洞，失败再降级」。

### Q2: 回落 TURN 后传 GB 文件的带宽成本是多少？

以 100GB 文件、亚太区为例：

| 方式 | 计费口径 | 估算成本 |
|---|---|---|
| Twilio TURN 亚太 | $0.60/GB × 双向 200GB | **~$120** |
| Cloudflare TURN | $0.05/GB × 200GB | ~$10 |
| 自建 coturn @ Hetzner | 含 20TB 流量 | **~€5-15/月**（100GB 无额外费用） |
| 自建 coturn @ 云厂商按流量 | ¥0.5-0.8/GB × 200GB | ~¥100-160 |
| croc 公共 relay | 免费（作者自担） | $0，但不可依赖 |
| 本项目 WS 中继 @ 同 VPS | 同 coturn | 同 coturn |

**关键结论**：TURN/relay 的成本差异不在协议，而在**你从谁那里买带宽**。自建 VPS 比云 TURN 便宜 1-2 个数量级。所以真正的决策是「是否愿意运维一台中继 VPS」，而不是「选哪个协议」。

另一个容易被忽略的点：**TURN 按双向计费**。很多成本估算只算单向，实际会翻倍。

## 3. 最终推荐

### 推荐：croc 用于「尽快能传文件」，本项目自研 `cmesh` 用于「可控的双向通道」

理由：

1. **croc 已经在本环境实测可用**，这是唯一被验证过的事实。如果目标只是「把文件从 A 传到 B」，直接用 croc，零开发。
2. 但 croc 不满足「小消息双向收发 + 长连接 + 自定义协议」的需求。本项目的 `cmesh` 用**与 croc 完全相同的网络形态**（双方出站 WebSocket → 中继 staple），因此继承了 croc 已验证的可行性，同时提供：
   - 双向消息通道（长连接）
   - 可自建 relay（一条命令）
   - 显式断点续传（`.part` + offset 协商）
   - 端到端加密（SPAKE2 + AES-256-GCM），relay 无法解密
3. **不选 iroh**：Go 绑定不可用于生产，本机无法验证，违背「真实跑通」的交付要求。
4. **不选 pion 自建**：本质是自建 TURN，工作量最大而收益与 WS 中继相同（本环境打洞大概率失败）。
5. **不选 Syncthing**：语义不匹配，常驻进程开销大。

### 演进路径（如果将来要优化）

1. 当前：WS 中继，全流量过 relay。
2. 加直连：在 relay 建立后，双方交换 STUN 得到的映射地址，尝试 UDP 打洞；成功则用 QUIC 或加密 UDP 替换数据面，relay 只做信令。这就是 iroh/Tailscale 的形态，但可以用 Go 自己实现（pion/ice 或手写）。
3. 如果团队接受 Rust：直接换 iroh，省掉上述全部工作。

## 4. 本项目的协议选择理由

| 决策 | 选择 | 理由 |
|---|---|---|
| 传输层 | WebSocket over TCP/TLS | 穿透 HTTP 代理与 CDN；无 100MB 请求体限制；天然消息分帧；croc 已验证同形态可行 |
| 加密 | SPAKE2 (pake/v3) + HKDF + AES-256-GCM | PAKE 抵抗离线字典攻击；短码即可安全；每方向独立密钥 + 单调计数器防重放 |
| 分块 | 64 KiB/frame | 远低于 1 MiB 帧上限，代理友好，续传粒度细 |
| 续传 | `.part` 文件 + offset 协商 + 全文件 SHA-256 | 简单、可验证；坏前缀最多浪费一次重传 |
| 中继 | 无状态转发（不解析内容） | relay 无法读流量；水平扩展只需加实例 |

## 5. 两个值得记录的异常现象

### 5.1 透明代理与 fake-IP

`github.com → 198.18.0.5` 属于 `198.18.0.0/15`（RFC 2544 基准测试网段），是 Clash/Surge 一类代理的 fake-IP 池。特征：
- TCP 握手 0.1ms（本地代理直接应答，不是真实 RTT）
- DNS 解析返回代理伪造地址

影响：
- **STUN 结果不可信**：UDP 可能被代理接管，返回的映射地址未必真实可达。
- **打洞几乎不可能成功**：代理通常不转发任意 UDP。
- **TCP/WS 中继不受影响**：代理对 TCP 是正常转发的，这正是 croc 和本项目能工作的原因。

### 5.2 EasyTier `--no-tun` 异常

现象：能监听端口，但连接 127.0.0.1 和 1.1.1.1:443 全部 2 秒超时；同样参数在 macOS 正常。

未查明原因（本任务未做进一步排查），但可以给出几个假设：
1. `--no-tun` 模式下仍尝试创建某种 socket 或设置 sockopt，被 seccomp 拦截后进入异常状态。
2. 容器内缺少某些 `/proc` 或 `/sys` 条目，导致其网络栈初始化半途失败。
3. 透明代理对非标准端口的 UDP/TCP 处理异常，而 EasyTier 的心跳恰好命中。

**这也说明一个方法论**：在这个容器里，「能监听端口」不等于「能出站连接」，权限探测必须实测出站，不能只看 bind 成功。本项目的 `probe` 模块正是按这个原则设计的。

## 6. 未验证 / 假设清单

| 项 | 状态 |
|---|---|
| croc 在容器内可用 | ✅ 主代理实测 |
| 本项目 `cmesh` 在目标容器内可用 | ❌ **未验证**（本机无容器运行时；需在目标容器跑 `cmesh probe` 与端到端） |
| 本项目 relay 部署在公网后可达 | ❌ 未验证（仅本机回环验证） |
| 透明代理对 WebSocket 长连接的行为 | ⚠️ 假设可行（croc 用 TCP，本项目用 WS over TCP，风险相近但未实测） |
| 断点续传跨进程重启 | ✅ 本机验证（预置 `.part` 后成功从 offset 续传） |
| 1GB+ 文件 | ⚠️ 仅验证到 100MB（本地 182 MiB/s） |
| 双向同时传文件 | ❌ 未实现（当前协议是单向 offer/accept，双向需两个并发会话或协议扩展） |
| TUN 探测在真实无权限容器内的表现 | ⚠️ 逻辑已实现并单测，但本机是 macOS，未在 Linux 容器实跑 |

## 7. 参考

- Chrome UMA / WebRTC TURN 比例：15-30% 会话需要 relay，直连 75-80%。
- NAT 类型与打洞成功率：full-cone ~98%、address-restricted ~90%、port-restricted ~75%、symmetric ~20%。
- iroh 官方：打洞约 90%，95% 数据量走直连；relay 无状态。
- Twilio TURN 定价：US/EU $0.40/GB、亚太 $0.60/GB、悉尼/圣保罗 $0.80/GB。
- croc 作者自述：公共 relay 2023 年约 20TB/月，$40-50/月。
- Syncthing relay：社区 relay 普遍 <10Mbit；`strelaysrv` 默认加入公共 pool。

---

# 2026-10-18 第二轮：自动化组网能力（心跳 / 重连 / 多 relay / 双向传输）

目标：从「能传一次」升级为「常驻节点」——relay 或网络抖动后自动恢复、
多 relay 容灾、会话中随时双向传文件。以下为本轮的关键决定与被否决的方案。

## D8. 心跳放在 WS 传输层，而不是应用层 FramePing

**决定**：`wsconn` 对每条连接做 20s ping / 60s 读超时（pong 与数据都会刷新
deadline）；`FramePing/FramePong` 保留但不再主动使用。

**为什么不用应用层心跳**：
1. 发送方流式传文件时**只写不读**（SendFile 循环里没有 Recv），应用层 ping
   到达对端后没人应答，反而会被 relay 误判为死连接——会话读泵（见 D9）必须
   常驻读取，WS 层 pong 由 gorilla 在读循环内自动回复，天然解决这个问题。
2. relay 侧对每条腿也需要独立的存活判定，splice 只转发字节不解析协议，
   应用层心跳帮不了它。

**实现要点（都踩过坑）**：
- 读 deadline 在每次 `Recv` 进入时刷新 + pong handler 到达时刷新。不能只在
  handler 里刷新：无流量时读阻塞在 `ReadMessage` 上，pong 不来就没有刷新时机。
- **`Close()` 绝不能碰 `readMu`**：阻塞在 `ReadMessage` 上的读泵永远持有
  readMu，Close 若先取 closeMu 再取 readMu 就是必然死锁（e2e 实测抓到，
  见 `TestCloseUnblocksRecv` 回归测试）。stopLive 通道由 closeMu 保护。
- 一次只有一个 WS 帧在路上，单帧传输时间超过 60s（即持续速率 <10 KB/s）
  会被误杀——已知下限，README 已标注。

## D9. peer.Session 改为单一读泵 + 按帧类型分发

**决定**：会话内只有读泵一个 goroutine 读传输层；`FrameMsg` 走回调，
`FrameFileChunk/Done` 送入当前 `InboundTransfer`，`FrameFileAccept/Reject/Result`
送入 outbound 通道给 `SendFile` 等待者，`FramePing` 自动回 pong。

**为什么必须改**：旧 API 是 pull 式（调用方各自 `recvFrame`）。常驻模式下
读循环 + 用户 `/file` 命令 + 发送方等待 result 三者会并发调用 Recv——两个
goroutine 抢同一条连接是竞态，靠运气不崩。分发模型让两个传输方向完全独立：
发送方只写 chunk、从专属通道收 accept/result；接收方从专属通道收 chunk。
这也是「双向同时传一个文件」能成立的前提（`TestDuplexTransfers`）。

**API 取舍**：放弃 pull 式 `ReceiveFile/RecvFrame`，改为 `Handler.OnOffer`
回调（返回 bool 表示是否接管传输）。测试同步迁移。

## D10. 断线重连 = 客户端拨号循环 + relay「后来者踢占位」

**决定**：客户端 `Node.Run` 常驻循环——按 relay 列表顺序轮拨、指数退避、
每次重连完整重跑 SPAKE2（无会话状态可恢复，代码即身份）；relay 侧同房间
同角色的新连接**直接踢掉旧泊位**（last-writer-wins），配对中的僵尸连接
不阻塞重连。

**否决的方案**：relay 主动探测泊位连接是否存活。
1. 读探测：配对前客户端就可能发出 PAKE msg1（TCP 缓冲，splice 启动后转发），
   这是协议的合法行为——监测器要么把数据当违规杀连接（实测打挂全部转发
   测试），要么把偷读的数据缓冲回放（需要包装整个连接类型）。
2. deadline 循环读：gorilla 的读错误是 sticky 的，deadline 在帧边界超时一次
   就永久污染连接，不能用于轮询。
3. MSG_PEEK 原始 fd 探测：可行但要按平台分叉（unix/其他），复杂度不值。

踢占位方案零探测、零平台分叉：能重新出示 code 的新连接理应比僵尸优先
（code 本来就是唯一凭据）。代价是知道 code 的攻击者可以踢掉等待中的对方，
但知道 code 本身就能完成握手，攻击面没有扩大。

## D11. 文件分块 64 KiB → 512 KiB

每帧的固定开销（AEAD、WS 帧头、splice 两次拷贝）在真实广域网上是吞吐主导项。
回环实测 512 MiB：211.9 → 539.3 MiB/s（2.5×）。1 MiB 线上限内仍有余量，
但更大会放大慢链路上的队头阻塞（见 D8 的 10 KB/s 下限）。续传粒度从 64 KiB
变粗到 512 KiB，对 GB 级文件无关紧要。

## D12. 多 relay：有序列表 + 配对窗口轮换

**决定**：`--relay A,B,C` 按固定顺序尝试；连接失败或 `--pair-window`（默认
60s，一次性命令压到 10s）内对方未出现就换下一台；整轮失败退避后重试。

**收敛性论证**：两端用同一份列表，各自独立选「第一台活的」。分歧只发生在
健康状态视差（一端认为 A 活、另一端认为 A 死）——此时两端先后都会轮到 B
并配对，代价是一次 pair-window 的等待。部署上建议国内/海外各一台、两端
按自己区域优先排序列表（详见 deploy/README.md 的路由策略）。

**否决**：按 RTT 选最快 relay——两端可能选到不同的 relay 永不相遇，需要
中央协调或把 relay 编进 code，复杂度不成比例。

## D13. 一次性命令的有界性

常驻模式（`--stay`）一切重试无上限（守护语义）；一次性命令（`--stay=false`）
必须能自己退出：
- `MaxConnectAttempts=8`：拨号尝试上限；
- `MaxTransferAttempts=3`：offer 重试上限（配合 D14 的 offer 超时）；
- code 格式错误（如长度 <6）归类为 `ErrProtocol` **永久错误**，立即退出而非
  重试（实测发现短 code 会导致无限重试）。

## D14. SendFile 的两个等待点加超时

等 accept 30s（"没人收"变成错误而不是永恒等待），等 result 10min（此时
数据已在途，超时只防连接假死）。没有这两个超时，单端测试里发送方会永久
挂在 outboundCh 上。

## D15. probe STUN 扩为多服务器并行（含国内）

默认列表：`stun.miwifi.com`、`stun.qq.com`（国内）+ `stun.cloudflare.com`、
`stun.l.google.com`（海外），并行查询、逐个报告。实测（中国移动网络）：
miwifi 138ms 成功、qq 超时、Cloudflare/Google 可达但返回**不同的端口映射**
（24628/50063/58112）——单服务器探测会低估 NAT 限制，多服务器的分歧本身
就是诊断信息。报告新增 `network.stun[]` 结构化字段。

## D16. 部署形态

`Dockerfile`（多阶段、distroless、非 root）+ `deploy/`（compose + Caddy
自动 HTTPS + 免费资源矩阵：Oracle Always Free / Cloudflare Tunnel / Workers
DO / 国内轻量新用户）。免费选项的真实约束是**流量配额**（relay 转发量 ≈
文件大小 ×2），不是 CPU/内存。公网部署未实测，诚实标注在 deploy/README.md。

---

# 2026-10-19 第三轮：外部评审报告驱动的安全加固

依据外部评审（`container-mesh-review-2026-10-18.md`）逐条验证并修复。以下记录
每条决定的技术取舍，特别是**没有完全照搬报告建议**的地方。

## D17. room 改为 HMAC 派生 —— 但纯 HMAC 不足以防 MITM

**报告建议**：`room = HMAC(code, salt)`，让 relay 看不到 code。

**采纳，但补充了关键一点**：单纯改 room 只解决「relay 直接拿到 code」，
**并不阻止主动 MITM**——因为 relay 拿到 `RoomID(code)` 后仍可对弱 code 做
**离线字典攻击**（room 是 code 的确定性函数，等于一个 verifier）。一旦猜出
code，MITM 依然成立。

**因此同时引入 SAS**：
- `secure.RoomID(code)`：`HMAC-SHA256(normalize(code), "container-mesh/room/v1")`
  前 16 字节 hex。relay 只看到不可逆标签。
- `secure.Session.ShortAuth()`：6 位十进制，由 session key 经**独立 HKDF 标签
  `v1 SAS`** 派生。两端必须算出相同值。
- 连接建立时打印 `SAS=xxxxxx fingerprint=...`，由双方带外比对。

**踩到的坑**：最初用 `sendAEAD` 的密钥派生 SAS，导致两端数值不同——因为
A→B 与 B→A 是**不同方向**的密钥。必须从 session key 单独派生一个**对称**
的 SAS key。

**未做的**：没有实现自动 SAS 校验。自动校验需要预共享身份（证书/公钥），
那会把问题转移成另一套密钥分发。当前设计明确要求人工比对一次。

## D18. relay 限流：按 IP 而非按 room

**报告建议**：按源 IP/房间做令牌桶。

**采纳按 IP，拒绝按 room**：按 room 限流会误伤——同一 code 的两个合法端
本来就会各开一个连接，且重连会再开；按 room 限流需要区分「合法重连」与
「恶意 squatting」，而这两者在 relay 视角完全一样（都是同 room+role 的新连接）。
按 IP 限流则没有这个歧义，且能同时挡住「大量建房耗尽 MaxRooms」。

- 并发连接上限：默认 16/IP
- 新建速率：默认 120/分钟/IP（令牌桶，按秒补充）
- `X-Forwarded-For` / `X-Real-IP` 参与判定：生产部署几乎一定在 nginx/Caddy 后面，
  否则所有客户端共享代理的预算
- 空闲状态定期 sweep，避免 map 无限增长

**保留「后来者踢占位」**：它是断线重连能工作的前提（见 D10）。限流降低了
被滥用的成本，但没有消除——知道 code 的人仍可踢掉等待者。这是可用性问题，
不是机密性问题，且 SAS 会让真正的 MITM 暴露。

## D19. 接收端：上限 + 空间预检 + 不覆盖

**报告建议**：加 `--max-file-size` + `statfs` 检查 + 名字白名单。

**全部采纳**，几处实现细节：

- **默认 16 GiB，而非报告建议的 4 GiB**：本项目的目标场景包含大文件传输
  （README 里 1 GiB 是常规用例），4 GiB 会误伤。上限的意义是「防写满磁盘」，
  不是「限制合法用途」；真正的护栏是空间预检。
- **用 `Bavail` 而非 `Bfree`**：`Bfree` 包含 root 保留块，非 root 接收端会
  在预检通过后仍然写失败——而目标场景恰恰是非 root 容器。
- **名字白名单放宽到包含 `()[]@#`**：第一版只允许 `[A-Za-z0-9._+- ]`，测试
  立刻打脸——`My File (1).zip` 是极常见的下载文件名。这些都是安全的
  （不是路径分隔符、不是 shell 元字符）。
- **不覆盖用 link-then-remove**：`os.Rename` 会静默覆盖，等于让对端能销毁
  无关文件。`os.Link` 在目标存在时返回 EEXIST，天然是「不存在才创建」；
  不支持硬链接的文件系统（FAT/部分网络盘）回退为显式存在性检查。

**顺带修掉的真实 bug**：`publishNoClobber` 失败时原本直接 `return nil, err`，
**没有给发送方回 FileResult**，发送方会一直等到 10 分钟超时。测试
（`TestRefusesToOverwriteExistingFile`）以 40 秒超时暴露了它。现在发布失败
也会回一个 `OK:false` 的结果。

## D20. 删除 insecure TLS 选项（而非保留）

**报告建议**：删除或显式审计。

**选择删除**：`wsconn.DialOptions.InsecureSkipVerify` 没有任何 CLI 调用点，
属于「将来可能被误接上」的死代码。保留一个能静默关闭证书校验的开关，在
一个以「relay 不可信」为前提的项目里是自相矛盾的——TLS 校验保护的是
「relay 是不是真的 relay」，关掉它等于放弃唯一的路由层身份检查。
需要自签证书的场景应该用私有 CA，而不是跳过校验。

## D21. module 路径对齐仓库

`github.com/blackevil/container-mesh` → `github.com/muzimu217/container-mesh`。
之前不一致导致 `go install ...@latest` 不可用、外部按仓库地址 import 也对不上。
Makefile 里的 `-ldflags -X` 路径同步修改。

## D22. 错误不再静默吞掉

评审指出多处 `_ = SendFrame(...)` / `_ = os.Remove(...)`。影响不是崩溃，而是
**排障困难**：对端只会看到超时或「哈希不一致」，看不到「I/O 错误」。
现在这些路径至少 `log.Warn` 一行；关键路径（发布失败、哈希不符）确保
**一定给对端一个明确的 FileResult**，不让它空等。
