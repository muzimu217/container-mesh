# 多路复用与端口转发技术研究

- 研究日期：2026-10-20
- 研究对象：`container-mesh`（下称 cmesh）——在单条加密连接上做多路复用 + `ssh -L`/`ssh -R` 式端口转发
- 上游快照：`5226d96`（`fix: a zero pair window expired immediately with several relays`）
- 工作方式：**只读** cmesh 仓库；全部原型与实测在 `/tmp/cmuxlab` 下独立进行。已确认 `git status --porcelain` 为空，**未修改仓库任何文件**
- 结论口径：**已实测** = 本次在本机跑出的数字/行为；**查证** = 读源码或官方文档确认；**推断** = 基于已知事实的工程判断，未实测

---

## 0. 结论摘要

**推荐方案：`hashicorp/yamux`，插在 `secure` 之下、`wsconn`/`p2pquic` 之上（即"多路复用承载加密帧"）。**

一句话理由：cmesh 现有的 `secure.Session` 是一个**带严格递增计数器、且要求严格顺序**的 AEAD 帧通道——它的顺序性要求与"多路复用"天然冲突。把 yamux 放在 secure **之下**，让每个 stream 承载一串**已经加密好的帧**，就完全不需要改动 secure 的语义，也不需要给每个 stream 派生密钥。

| 决策 | 选择 | 依据 |
|---|---|---|
| 复用库 | **hashicorp/yamux** | 已实测：每流独立流控、有半关闭、20000 流稳定、零间接依赖、+33 KB 二进制 |
| 不选 smux | — | 已实测：**v1 会静默丢数据**（15 次跑 2 次复现，最多丢 22 MiB）；v2 表现正常但依赖更重、默认参数更保守 |
| 不选 QUIC 原生多流 | — | 已实测：吞吐低一个数量级（139–193 MiB/s vs 1962–2748 MiB/s），且只在 p2p 直连时可用，relay 路径（WebSocket）无法复用 |
| 加密边界 | **整条连接加密，复用在内层** | secure 的计数器语义要求单写者单读者；yamux 恰好只有一个 `sendLoop` 和一个 `recvLoop`（已查证 `session.go:488,514`） |
| 端口转发表达 | `cmesh forward -L/-R` 子命令 + 独立白名单 | 已实测原型：10/10 并发连接正确、白名单拒绝生效 |

**主要风险（按严重度）：**

1. **开放代理**——如果 `-R` 的接受端不限制目标，任何拿到 code 的人都能把容器变成进入内网的跳板。**这是本方案唯一的高危风险**，必须默认拒绝。
2. **内存**——每条活跃流约占 `2 × MaxStreamWindowSize`（已实测 514 KiB @256 KiB 窗口）。1000 条并发转发 ≈ 500 MiB。必须有流数上限。
3. **重连语义**——cmesh 的 `Node.Run` 会自动重连并重建 session；所有已建立的 stream 会在重连时全部断开。转发需要"连接级重试"，不能假设隧道长存。

---

## 1. 多路复用库选型

### 1.1 三个候选的定位

| | hashicorp/yamux | xtaci/smux | QUIC 原生流 |
|---|---|---|---|
| 版本 | v0.1.2（2024-09） | v1.5.57 | quic-go v0.55.0（已在用） |
| 许可证 | MPL-2.0 | MIT | MIT |
| 依赖 | **零**（仅 stdlib） | 零 | 已引入 |
| 传输要求 | 可靠有序字节流 | 可靠有序字节流 | 自带可靠性（UDP） |
| 流控 | **每流窗口**（默认 256 KiB） | v1: 会话共享令牌桶；v2: 每流滑动窗口 | 每流 + 连接级 |
| 半关闭 | `Close()` 即半关闭（读仍可用） | `CloseWrite()` 显式 | `Close()` / `stream.CancelRead` |
| 头部开销 | 12 B | 8 B | 可变长（quic varint） |
| 被依赖数 | 1,681 个包（pkg.go.dev，查证） | kcptun 等（README 自述） | 极广 |

### 1.2 hashicorp/yamux

**API（查证，`mux.go` / `session.go` / `stream.go`）：**

```go
func Client(conn io.ReadWriteCloser, config *Config) (*Session, error)
func Server(conn io.ReadWriteCloser, config *Config) (*Session, error)

func (s *Session) OpenStream() (*Stream, error)
func (s *Session) AcceptStream() (*Stream, error)
func (s *Session) AcceptStreamWithContext(ctx context.Context) (*Stream, error)
func (s *Session) NumStreams() int
func (s *Session) Ping() (time.Duration, error)
func (s *Session) GoAway() error          // 停止接受新流，不关连接
func (s *Session) Close() error
func (s *Session) CloseChan() <-chan struct{}

func (s *Stream) Read([]byte) (int, error)   // 实现 io.Reader
func (s *Stream) Write([]byte) (int, error)  // 实现 io.Writer
func (s *Stream) Close() error               // 半关闭写侧
func (s *Stream) StreamID() uint32
func (s *Stream) SetDeadline(t time.Time) error
```

`Config` 的关键字段（`mux.go:17-55`）：

```go
type Config struct {
    AcceptBacklog          int           // 默认 256：等待 Accept 的流上限
    EnableKeepAlive        bool          // 默认 true
    KeepAliveInterval      time.Duration // 默认 30s
    ConnectionWriteTimeout time.Duration // 默认 10s：写不出去就认为连接坏了
    MaxStreamWindowSize    uint32        // 默认 256 KiB，必须 >= 256 KiB
    StreamOpenTimeout      time.Duration // 默认 75s：开流无 ACK 就关整个 session
    StreamCloseTimeout     time.Duration // 默认 5min：半关闭后等对端 FIN 的上限
    LogOutput              io.Writer     // 默认 os.Stderr（注意：会污染 CLI 输出）
    Logger                 Logger
}
```

**成熟度（查证 + 推断）：**

- 协议有正式 spec（`spec.md`），帧格式固定 12 B 头：`Version(1) Type(1) Flags(2) StreamID(4) Length(4)`。
- 被 1,681 个包导入（pkg.go.dev 查证）。HashiCorp 生态（Consul/Vault/Nomad 的连接复用）是其设计目标，但 pkg.go.dev 页面本身不列举具体使用者，**"被某产品使用"这一点本次未查证到直接证据**。
- 版本号仍是 `v0.x`，API 有 breaking change 的历史；但协议本身（spec.md）稳定，且该库是"小到可以整体抄走"的体量（约 2,900 行含测试）。

**限制（全部已查证源码）：**

1. **`MaxStreamWindowSize` 下限是 256 KiB**（`mux.go:83-85` 显式校验）。想调小以减少内存是做不到的——这直接决定了内存下限。
2. **没有 `CloseWrite()`**。但 `Close()` 实际就是半关闭：`stream.go:324-378` 中 `Close()` 只把状态置为 `streamLocalClose` 并发 FIN，`Read` 在 `streamLocalClose` 状态下仍正常返回数据（`stream.go:100-104`）。**已实测**：`Close()` 之后仍能读到对端的 18 字节响应。
3. **`StreamOpenTimeout` 失败会关掉整个 session**（`session.go:213-230`），不是只关那一条流。对端口转发意味着：一条流的 SYN 没被 ACK，整条隧道重来。
4. **`LogOutput` 默认是 `os.Stderr`**（`mux.go:70`），且 `VerifyConfig` 要求 `LogOutput` 和 `Logger` **二选一非空**（`mux.go:89-95`）。接入时必须显式设成 `io.Discard` 或自己的 logger，否则会往 CLI 的 stderr 里吐 yamux 内部日志。
5. **单个 `sendLoop` 串行化所有流的写**（`session.go:472-529`）。这是流控正确性的来源，也是**队头阻塞的潜在来源**——但只在"某条流的 body 写入慢"时成立，而 body 写入慢意味着底层连接本身慢。已实测 50 并发流时聚合吞吐 2748 MiB/s，未观察到饿死。
6. **每帧两次 `conn.Write`**（先 header 12 B，再 body，`session.go:488` 与 `:514`）。对 WebSocket 这种"一次 Send = 一条消息"的传输层，**消息数翻倍**。已实测：64 MiB 数据产生 801 条传输消息（约 13 条/MiB，平均 81.8 KiB/条）——因为 yamux 的 body 写入会被 `MaxStreamWindowSize` 切成大块，实际影响可接受。

### 1.3 xtaci/smux —— 为什么不推荐

**API 差异（查证）：**

```go
func Client(conn io.ReadWriteCloser, config *Config) (*Session, error)
func Server(conn io.ReadWriteCloser, config *Config) (*Session, error)
func (s *Session) OpenStream() (*Stream, error)
func (s *Session) AcceptStream() (*Stream, error)
func (s *Session) NumStreams() int
// Stream 额外提供：
func (s *stream) CloseWrite() error   // 显式半关闭，比 yamux 更符合直觉
```

`Config` 与 yamux 的语义差别很大：

```go
type Config struct {
    Version          int           // 1 或 2；默认 1
    KeepAliveDisabled bool
    KeepAliveInterval time.Duration // 默认 10s
    KeepAliveTimeout  time.Duration // 默认 30s
    MaxFrameSize      int           // 默认 32768，上限 65535
    MaxReceiveBuffer  int           // 默认 4 MiB —— 会话级共享
    MaxStreamBuffer   int           // 默认 64 KiB —— 每流上限（v2 生效）
}
```

**致命问题：v1 会静默丢数据（已实测，可复现）。**

`stream.go:490-530` 的 `writeV1` 实现里，**完全没有读 `peerWindow`**：

```go
func (s *stream) writeV1(b []byte) (n int, err error) {
	...
	for len(b) > 0 {
		size := len(b)
		if size > s.frameSize { size = s.frameSize }
		frame.data = b[:size]
		n, err := s.sess.writeFrameInternal(frame, deadline, CLSDATA)
		atomic.AddUint32(&s.numWritten, uint32(size))
		sent += n
		if err != nil { return sent, err }
		b = b[size:]
	}
	return sent, nil
}
```

对比 `writeV2`（`stream.go:533-640`）有完整的 `win := peerWindow - inflight` 检查与阻塞等待。**v1 的 `Write()` 无条件返回成功**，而接收端 `recvLoop` 只在会话令牌桶（`MaxReceiveBuffer`，默认 4 MiB）耗尽时停止读 socket——此时数据积压在内核 socket 缓冲区，一旦 `streamClosed` 被调用，`recycleTokens()`（`stream.go:766-776`）会**清空该流的 bufferRing 并丢弃数据**，同时把 token 还给会话。

实测（`/tmp/cmuxlab/droptest`，64 MiB，接收端停顿 2s 后开始读）：

```
smux1  writer: 67108864 bytes accepted in 2.065s
       reader: 61865984 bytes, err=io: read/write on closed pipe
       ==> DATA LOSS: 5242880 bytes accepted by Write() never arrived
```

15 次重复：**2 次失败**（丢 22.0 MiB 与 8.9 MiB）。丢包率不高，但**是静默的**——`Write` 返回了 `nil`。对端口转发（隧道里跑的是 HTTP、数据库协议、SSH）这意味着随机的、不可检测的字节流损坏。

smux v2 实测正常（256 KiB 处正确阻塞，完整性 PASS），但既然 v2 能用，就没有理由为了省 4 字节头部去承担"选错版本就静默损坏"的风险；而且它的 `MaxFrameSize` 上限 64 KiB 比 yamux 的窗口机制更碎。

**其他差异：**

| | yamux | smux |
|---|---|---|
| 接收缓冲模型 | 每流独立窗口 | 会话共享令牌桶 + 每流上限 |
| 内存可预测性 | 每流固定 ≈2×window | 会话总量受控，单流可能被其他流挤占 |
| 半关闭 | `Close()` 隐式 | `CloseWrite()` 显式 |
| 队头阻塞风险 | 单 sendLoop | 内置 fair-queue shaper（`shaper.go`） |
| 实测吞吐（1/10/50 流，MiB/s） | **3211 / 2109 / 2771** | 1452 / 1701 / 1760 |

yamux 在吞吐上领先约 1.6–2.2 倍（已实测，loopback，128 MiB/流）。

### 1.4 QUIC 原生多流 —— 不能替代

项目已有 `internal/p2pquic`，`quic.Conn` 天然支持 `OpenStreamSync` / `AcceptStream`，看起来"不用额外库"。

**但它解决不了本需求，原因有三（已查证 + 已实测）：**

1. **只在 p2p 直连时存在。** `p2pquic` 依赖打洞成功（`internal/cli/node.go:359-423`：`Negotiate` → 拿到 UDP socket → 在其上跑 QUIC）。而 relay 路径是 WebSocket（`internal/wsconn`），**QUIC 无法承载 WebSocket**。cmesh 的默认部署（`--p2p` 默认 false，见 `internal/cli/cli.go:132-133` 的注释）就是 relay-only。也就是说，用 QUIC 做复用等于"端口转发只在打洞成功时可用"，这与该功能的主要用途（穿透受限网络）矛盾。
2. **吞吐低一个数量级（已实测）。** 同样 512 KiB 写块、loopback：

   ```
   quic    streams=1       193.2 MiB/s
   quic    streams=10      139.0 MiB/s
   quic    streams=50      155.2 MiB/s
   ```

   对比 yamux 在"消息型传输 + 每消息 AES-GCM"下的 1962–2748 MiB/s。原因是 quic-go 的 per-stream 窗口默认较小（`MaxStreamReceiveWindow` 未调），且每个 512 KiB 写被切成约 1200 B 的包、走用户态加密与拥塞控制。
3. **现有 `p2pquic.Conn` 的设计恰恰是"一条流跑所有消息"**（`conn.go:68-80` 的注释明确写了：一条流保持顺序，开多流会让 `AcceptStream` 打乱顺序）。要改成多流，等于重写该包，而它当前的**单流设计是刻意的**——因为上层 `peer.Session` 要求严格顺序。

**结论：QUIC 的流可以用于"直连时的额外优化"，但不能作为端口转发的复用基础。**

### 1.5 内存开销与最大并发流（已实测）

| 指标 | yamux | smux v1 | smux v2 |
|---|---|---|---|
| 空闲流 | **1.7–2.5 KiB/流** | 2.1 KiB/流 | 同 v1 |
| 缓冲填满的活跃流 @256 KiB 窗口 | **514.8 KiB/流** | 不受控（会话桶） | ≈窗口大小 |
| 缓冲填满的活跃流 @1 MiB 窗口 | **2.0 MiB/流** | — | — |
| 打开耗时 | 20000 流 177–254 ms | 2000 流 17–19 ms | — |
| 20000 流时库自报 `NumStreams` | 20000（一致） | — | — |

关键数字：**每活跃流 ≈ 2 × `MaxStreamWindowSize`**（发送侧窗口 + 接收侧窗口同时存在）。这个 2 倍关系实测精确（256 KiB → 514.8 KiB；1 MiB → 2.0 MiB）。

yamux 的流 ID 是 `uint32` 且按 2 递增（`session.go:170-180`），理论上限约 2^31 条流；**不存在实际的并发流数硬上限**（`AcceptBacklog` 只限制"已 SYN 但未被 Accept"的数量，默认 256）。已实测 20000 并发流稳定。

**推断**：以 256 KiB 窗口、每条活跃流转发占用 515 KiB 计，一个 1 GiB 内存预算的容器可以支撑约 2000 条同时活跃的转发连接。这远超实际需要，但必须设上限（见 §3.4）。

---

## 2. 与现有架构的契合度

### 2.1 当前分层（查证）

```
cli.Node
  └── peer.Session          ← 一条流跑所有帧；readPump 单读者分发
        ├── secure.Session  ← 每帧 AES-GCM，严格递增计数器，要求严格顺序
        └── switchable      ← 可在 relay WS 与 direct UDP 之间切换
              ├── wsconn.Conn    (relay: WebSocket, 消息型)
              ├── p2pquic.Conn   (direct: QUIC 单流, 消息型)
              └── p2p.Conn       (direct legacy, 消息型)
```

`peer.Session` 的核心约束（`internal/peer/session.go:4-21` 的包注释 + `:423-431`）：

- **只有一个 goroutine 读**（`readPump`），按 `Frame.Type` 分发。
- `SendFrame` 用 `sendMu` 保护 `SealFrame` + `Send` 的原子性（`session.go:423-431`）。注释明确说明：**如果两个 goroutine 并发 Seal 再竞争写 socket，高计数器的帧可能先上线路，接收端就会因计数器不匹配而失败**。
- `secure.OpenFrame` 要求帧**严格按发送顺序**到达（`secure.go:399-417`），否则视为协议违规（同时也是防重放）。

### 2.2 应该插在哪一层

**结论：插在 `secure` 之下，`wsconn`/`p2pquic` 之上。** 即：

```
peer.Session (业务帧)
  └── secure.Session          ← 整条连接一个密钥，计数器跨所有 stream 共享
        └── mux (yamux)       ← ★ 新增：每个 stream 承载一串已加密帧
              └── switchable
                    ├── wsconn.Conn
                    └── p2pquic.Conn
```

**为什么不是"每个 stream 独立加密"（即 mux 在 secure 之上）：**

1. **会破坏 secure 的现有语义。** `secure.Session` 的计数器是单一递增的；如果每个 stream 一个 `secure.Session`，要么每条流单独跑一次 SPAKE2 握手（多 RTT + 多一次 PAKE，代价大且要改 `Handshake`），要么从同一个 master key 派生 N 组子密钥（要改 `DeriveKeys`，且**必须保证 nonce 空间不重叠**，否则 AES-GCM 的 nonce 重用是灾难性的）。
2. **收益为零。** 整条连接已经在 TLS/WebSocket 之上、且被 SPAKE2 派生的 AES-256-GCM 保护。再给每条流套一层 AEAD 不增加任何安全性——攻击者能看到的仍然只是"一条加密连接"。
3. **改动量最小。** 在 secure 之下插入 mux，`peer.Session` 和 `secure.Session` **一行都不用改**：`peer.Session.conn` 字段的类型是 `MessageConn` 接口（`session.go:48-55`），而 yamux 需要的是 `io.ReadWriteCloser`。中间加一个 ~60 行的适配器即可。

**关键可行性依据（已查证源码）：** yamux 对底层连接**只有一个写者**（`sendLoop`，`session.go:472-529`，所有写入都走 `sendCh`）和**一个读者**（`recvLoop`）。这恰好满足 `secure.Session` 对单写者单读者的隐含要求——但注意方向：**是 yamux 满足 secure 的要求，而不是反过来**。所以适配器必须是"yamux 的字节流 → 消息流"方向，且必须在**加密之前**做消息化。

### 2.3 加密边界：推荐"整条连接加密，复用在内层"

| 方案 | 密钥数 | 握手次数 | 改动 | 安全增益 |
|---|---|---|---|---|
| **A. 整连接加密 + 内层复用（推荐）** | 1 组 | 1 次 | 适配器 ~60 行 | 无变化 |
| B. 每 stream 独立派生密钥 | 1 master + N 子密钥 | 1 次 | 改 `DeriveKeys` + 每流 `secure.Session` | **无** |
| C. 每 stream 独立 SPAKE2 | N 组 | N 次 | 大 | **无** |

**方案 A 的唯一代价**：所有 stream 的帧共享同一个计数器空间，因此**必须**保证 seal 与 send 在同一个临界区（现有 `sendMu` 已做到）。这也是为什么适配器要放在 secure 之下：secure 仍然只看到"一串有序的消息"。

### 2.4 传输切换（p2p 升级）的影响

`switchable`（`internal/peer/switchable.go`）允许在 session 运行中把底层传输从 relay WS 换成 QUIC。**如果 yamux 插在 switchable 之上，切换会直接破坏 yamux**——因为 yamux 在旧 socket 上已有未完成的帧状态，换 socket 后 `recvLoop` 会读到半个帧。

**两个可行做法：**

1. **推荐：mux 在 switchable 之下**，即 `secure → mux → switchable`。这样切换对 yamux 透明（yamux 看到的是同一个 `io.ReadWriteCloser`，底层实现换了）。但这要求 switchable 实现 `io.ReadWriteCloser`，而它现在是 `MessageConn`（`Send`/`Recv`）——**语义不匹配**。
2. **更简单：把 mux 放在 switchable 之上，但禁止在 session 运行中切换。** 即 p2p 协商必须发生在 mux 建立**之前**（当前 `Negotiator` 正是这个时机，`session.go:260-268`）。切换完成后才创建 yamux session。这要求 `Negotiator` 与 mux 创建的顺序是：握手 → hello → p2p 协商 + 切换 → **然后**建 mux → 启动 readPump。

**推断**：做法 2 改动最小，且符合现有代码结构（`StartWithNegotiation` 已经在 hello 之后、readPump 之前留了钩子）。代价是 p2p 切换后**不能再次切换**——但现有实现本来也只支持切换一次（`switchable.Switched()`）。

### 2.5 适配器设计草案（API 级）

```go
// internal/mux/adapter.go

// messageStream adapts a message-oriented transport (Send/Recv of whole
// messages) to the byte stream yamux requires. Each Write becomes exactly one
// transport message; Read reassembles from the current message.
//
// Invariant: Write is called only from yamux's single sendLoop, so no
// interleaving can occur. Read is called only from yamux's single recvLoop.
type messageStream struct {
    conn   peer.MessageConn   // Send([]byte) error / Recv() ([]byte, error)
    remain []byte             // unread tail of the last message
}

func NewMessageStream(c peer.MessageConn) io.ReadWriteCloser

func (m *messageStream) Read(p []byte) (int, error)
func (m *messageStream) Write(p []byte) (int, error)  // copies p, then one Send
func (m *messageStream) Close() error
```

```go
// internal/mux/session.go

// Config tunes the multiplexed session.
type Config struct {
    // MaxStreams bounds concurrently open streams. Zero means DefaultMaxStreams.
    MaxStreams int
    // Window is MaxStreamWindowSize. Zero means DefaultWindow (256 KiB, the
    // yamux floor). Memory per active stream is about 2*Window.
    Window uint32
    // KeepAliveInterval maps to yamux KeepAliveInterval.
    KeepAliveInterval time.Duration
    // Logger receives yamux diagnostics. yamux defaults to os.Stderr, which
    // would corrupt CLI output, so this is always set.
    Logger *slog.Logger
}

const DefaultMaxStreams = 256
const DefaultWindow = 256 << 10

// Session is a multiplexed, encrypted-agnostic stream container.
type Session struct {
    y *yamux.Session
    cfg Config
    sem chan struct{}   // bounds MaxStreams
}

// NewClient wraps the initiator side. The caller must already have completed
// the SPAKE2 handshake and any p2p upgrade.
func NewClient(c peer.MessageConn, cfg Config) (*Session, error)

// NewServer wraps the responder side.
func NewServer(c peer.MessageConn, cfg Config) (*Session, error)

// Open opens one stream, blocking while MaxStreams is reached.
func (s *Session) Open(ctx context.Context) (*Stream, error)

// Accept returns the next inbound stream.
func (s *Session) Accept() (*Stream, error)

func (s *Session) NumStreams() int
func (s *Session) Close() error

// Stream is one logical byte stream.
type Stream struct {
    st  *yamux.Stream
    sem chan struct{}
}

func (s *Stream) Read([]byte) (int, error)
func (s *Stream) Write([]byte) (int, error)
// CloseWrite half-closes: yamux has no CloseWrite, but Stream.Close is exactly
// a FIN on the write side and leaves reads working.
func (s *Stream) CloseWrite() error
func (s *Stream) Close() error
func (s *Stream) SetDeadline(t time.Time) error
```

**注意 `Stream.CloseWrite()` 的实现细节**（已实测）：yamux 的 `Close()` 只发 FIN、不释放流，读侧继续可用；但 `Close()` 之后再次 `Write` 会返回 `ErrStreamClosed`。因此 `CloseWrite()` 可以直接调 `st.Close()`，而 `Close()` 需要额外记录状态避免重复调用。**这是一个容易写错的地方**：如果适配器把 `Close()` 直接映射到 yamux 的 `Close()`，那么"读循环结束 → Close → 写循环也结束"的顺序是对的，但"先 Close 再 Read"仍然有效——已实测确认。

---

## 3. 端口转发的设计

### 3.1 语义对照

| | `ssh -L` | `ssh -R` |
|---|---|---|
| 谁监听 | **本地**（cmesh 进程所在容器） | **远端**（对端容器） |
| 谁拨号 | 远端 | 本地 |
| 数据方向 | 本地应用 → 本地端口 → 隧道 → 远端拨 target | 远端应用 → 远端端口 → 隧道 → 本地拨 target |
| 典型用途 | "我要访问对面的服务" | "让对面访问我的服务"（穿透 NAT） |

cmesh 的 `Node` 是**对称**的（A/B 只是 PAKE 角色，`internal/cli/cli.go:106-141` 的 `commonFlags` 两边共用），所以两种方向都能在同一套 session 上表达。

### 3.2 线协议：流前置头

每条转发的 stream 以一个小前置头开始，告诉接受端"拨哪里"。这正是 ssh 的 `direct-tcpip` channel 的做法。

```
+--------+----------+------------------+
| kind 1 | len 1    | addr len bytes   |
+--------+----------+------------------+
  1 = direct (ssh -L 方向：请拨 addr)
  2 = remote (ssh -R 方向：我是回连，请拨 addr)
```

```go
// internal/forward/proto.go

// Preamble opens every forwarded stream.
type Preamble struct {
    Kind byte   // KindDirect or KindRemote
    Addr string // "host:port"
}

const (
    KindDirect byte = 1 // the acceptor should dial Addr
    KindRemote byte = 2 // the opener is a -R listener; the acceptor dials Addr
)

// Status is the acceptor's one-word reply: 0 = dialled, non-zero = refused.
// Without it the opener cannot distinguish "peer refused" from "target is
// slow", and would have to guess from a closed stream.
type Status uint32

const (
    StatusOK        Status = 0
    StatusDenied    Status = 1 // allow-list rejected the target
    StatusDialFail  Status = 2 // target unreachable
    StatusTooMany   Status = 3 // stream limit reached
)

func WritePreamble(w io.Writer, p Preamble) error
func ReadPreamble(r io.Reader) (Preamble, error)
```

**为什么需要显式 `Status`（已实测的教训）**：原型里如果不回状态码，被拒绝的连接只能表现为"流立刻关闭"，客户端无法区分"策略拒绝"和"目标挂了"，日志里也看不出原因。加上 `Status` 后，实测输出从"连接被重置"变成明确的 `peer refused target ... (status 1)`。

### 3.3 双向握手时序

**`ssh -L`（本地监听 → 远端目标）：**

```
本地 cmesh                         远端 cmesh
    │                                   │
    │  accept() on 127.0.0.1:8080       │
    │─── OpenStream() ─────────────────>│
    │─── Preamble{Kind=1, Addr="db:5432"}│
    │                                   │  allowList.Permits("db:5432")?
    │                                   │  net.Dial("tcp", "db:5432")
    │<── Status(0) ─────────────────────│
    │<──────────── bidirectional bytes ────────────>│
```

**`ssh -R`（远端监听 → 本地目标）：**

```
本地 cmesh                         远端 cmesh
    │                                   │
    │                                   │  listen on 0.0.0.0:9000
    │<── OpenStream() ──────────────────│
    │<── Preamble{Kind=2, Addr="x:y"}───│   （Addr 仅用于日志/审计）
    │  allowList.Permits("x:y")?        │
    │  net.Dial("tcp", "x:y")           │
    │─── Status(0) ────────────────────>│
    │<──────────── bidirectional bytes ────────────>│
```

**关键点：两种方向都由"接受端"做 allow-list 检查，且检查的是它自己要拨的地址。** 这保证了策略永远在"实际发起连接的那一侧"执行——这是防开放代理的结构性保证，而不是靠约定。

### 3.4 访问控制：如何防止变成开放代理

**这是本方案唯一的高危风险，必须默认关闭。**

**威胁模型：** cmesh 的信任模型是"知道 code 的人就是合法对端"（`secure.go:4-15`）。但 code 可能泄露（截图、日志、共享屏幕），而**端口转发的破坏力远大于传文件**：拿到 code 的人可以把容器当作进入内网的跳板，扫描并访问容器能到达的任何地址。

**四层防御（全部默认拒绝）：**

**第 1 层：默认不启用。** 不接受转发，除非显式传 `--allow-forward`。与现有 `--p2p` 默认 false（`cli.go:132-133`）、`--allow-hidden` 默认 false 的风格一致。

**第 2 层：目标白名单（结构性）。**

```go
// internal/forward/allow.go

// AllowList decides whether a target may be dialled.
//
// An empty list permits NOTHING. This is deliberate: the zero value must be
// the safe value, because a forgotten configuration otherwise turns the
// container into an open proxy.
type AllowList struct {
    Hosts []string // exact hostname, or CIDR, or "localhost"
    Ports []int    // empty means "any port" (only meaningful with Hosts set)
}

func (a AllowList) Permits(addr string) error
```

实测行为：

```
[21:56:23.280] REFUSED 127.0.0.1:22: port 22 not in allow-list [19090]
[21:56:23.280] allow-list check: peer status=1 (1 = refused, expected)
```

**第 3 层：监听地址约束。** `-R` 的远端监听默认绑 `127.0.0.1`，**不绑 `0.0.0.0`**。要绑通配地址必须显式 `--listen-all` 并打印警告。ssh 的 `GatewayPorts no` 是同一个设计。

**第 4 层：流数与速率上限。**

```go
// Limits bounds what one session may consume.
type Limits struct {
    MaxStreams     int           // 默认 256；超出则 StatusTooMany
    MaxStreamsPerSec int         // 默认 32；防连接风暴
    DialTimeout    time.Duration // 默认 5s；防半开连接堆积
    MaxLifetime    time.Duration // 默认 0（不限）；0 之外的流到点强制关闭
}
```

**第 5 层（推断，建议但非必须）：SAS 确认。** cmesh 已有 `ShortAuth()`（6 位数字，`secure.go:292-303`）和 `--expect-sas`。**建议在启用端口转发时强制要求 `--expect-sas`**——因为转发是"持续的双向通道"，一旦被 MITM 长期利用，后果比一次性传文件严重得多。这与现有 `D38. SAS 强制比对` 的决策方向一致。

**绝对不要做的事：**

- ❌ 不要提供 `--allow-any` 这类"一键放开"的开关。需要放宽时应该写具体的 CIDR。
- ❌ 不要让白名单接受 `0.0.0.0/0` 或 `::/0` 而不打警告。
- ❌ 不要在 `-R` 模式下允许远端指定"回连到 opener 的任意地址"——`-R` 的目标地址必须由**本地**配置决定（`-R` 的参数），而不是由远端在 preamble 里随意指定。这是 ssh 与 cmesh 的一个语义差异：ssh 的 `-R` 中远端只负责 accept，目标由本地 `-R` 参数固定；cmesh 应照做，**preamble 里的 `Addr` 在 `-R` 模式下只用于日志，实际拨号地址来自本地配置**。

### 3.5 CLI 设计

```bash
# ssh -L 等价：本地 8080 → 对端拨 db:5432
cmesh forward --code CODE -L 127.0.0.1:8080:db:5432

# 多条转发，一条隧道
cmesh forward --code CODE \
    -L 127.0.0.1:8080:db:5432 \
    -L 127.0.0.1:9090:api:80

# ssh -R 等价：对端监听 9000，回连本地 127.0.0.1:3000
cmesh forward --code CODE -R 127.0.0.1:9000:127.0.0.1:3000

# 接受端必须显式声明允许的目标（默认拒绝一切）
cmesh forward --code CODE --accept \
    --allow-host db.internal --allow-host 10.0.0.0/8 \
    --allow-port 5432 --allow-port 6379

# 把转发和现有能力一起用（同一个 session）
cmesh forward --code CODE -L ... --also-recv-files --out ./inbox
```

**参数表：**

| 参数 | 默认 | 说明 |
|---|---|---|
| `-L [bind:]port:host:port` | — | 本地监听 → 远端目标；可重复 |
| `-R [bind:]port:host:port` | — | 远端监听 → 本地目标；可重复 |
| `--accept` | false | 本端接受对端发起的转发流 |
| `--allow-host HOST/CIDR` | 空（拒绝一切） | 可重复 |
| `--allow-port PORT` | 空（任意） | 可重复 |
| `--listen-all` | false | `-R` 时绑 `0.0.0.0`（默认仅 loopback） |
| `--max-streams N` | 256 | 并发流转发上限 |
| `--dial-timeout D` | 5s | 拨号超时 |
| `--expect-sas 123456` | 空 | 启用转发时**强烈建议**填写 |

**为什么复用现有 `--relay/--code/--name`：** `commonFlags`（`cli.go:106-141`）已经统一了这些参数与 `CMESH_RELAY`/`CMESH_CODE` 环境变量，`forward` 子命令应当直接复用，避免第四套连接参数。

**`-L` 的解析歧义（必须处理）：** `-L 8080:db:5432`（省略 bind）与 `-L 127.0.0.1:8080:db:5432`（带 bind）都合法。IPv6 地址必须用 `[::1]:8080:db:5432`。建议用 `net.SplitHostPort` 的变体按"从右往左切两段 host:port，剩下的作为 bind"来解析，并写表驱动测试覆盖：`8080:h:1`、`1.2.3.4:8080:h:1`、`[::1]:8080:h:1`、`8080:[::1]:1`。

### 3.6 与现有 `Node` 的集成点

```go
// internal/cli/node.go 的扩展（草案）

type NodeConfig struct {
    // ... 现有字段 ...

    // Forwards are local listeners to install once a session exists.
    Forwards []ForwardSpec
    // Accept enables serving inbound forward streams.
    Accept bool
    // Allow gates inbound forward targets. Nil means refuse everything.
    Allow *forward.AllowList
    // Limits bounds inbound forwarding.
    Limits forward.Limits
}
```

**生命周期问题（推断，必须在实现时处理）：**

`Node.Run` 在断线后会重建 `peer.Session`（`node.go:136-246`），**所有 yamux stream 都会随之失效**。因此：

- `-L` 的本地 listener 应当**只建一次**，而不是每次重连重建（否则端口会短暂不可用、且并发 bind 会失败）。
- 每条已接受的本地连接，在隧道断开时应当**关闭**（不是无限重试），并让应用层看到 EOF——这符合 TCP 代理的直觉，也避免"连接半开挂着"。
- 重连后新连接自然走新 session。**不需要**在协议层做 stream 级 resume（那会把复杂度推高一个量级，而收益很小）。

---

## 4. 实测验证

所有代码在 `/tmp/cmuxlab/`，均为独立 module（`lab`），**未触碰 cmesh 仓库**。

### 4.1 最小 demo：10 条并发流，一条 TCP 连接

`/tmp/cmuxlab/muxlab/main.go`，`-cmd=demo`。每条流发送 8 轮 × 512 KiB（等于 `peer.ChunkSize`）的**流专属确定性字节模式**，服务端 echo 时每字节 +1，客户端逐字节校验。任何跨流串扰或重排序都会在具体字节偏移处报错。

```go
// 核心：每条流的模式由 streamID 决定，字节级可验证
func patternFor(streamID int, size int) []byte {
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b[0:4], uint32(streamID))
	for i := 4; i < size; i++ {
		b[i] = byte((streamID*37 + i) & 0xff)
	}
	return b
}

// 服务端：读什么回什么，每字节 +1（反射 bug 立刻暴露）
for {
    n, err := st.Read(buf)
    if n > 0 {
        for i := 0; i < n; i++ { buf[i]++ }
        st.Write(buf[:n])
    }
    if err == io.EOF { return }
    if err != nil { return }
}

// 客户端：校验每个字节
for j := range got {
    if got[j] != want[j]+1 {
        return fmt.Errorf("stream %d round %d: byte %d is %02x, want %02x "+
            "(cross-stream contamination or reordering)", id, r, j, got[j], want[j]+1)
    }
}
```

**结果（yamux）：**

```
=== demo: 10 concurrent streams over ONE TCP connection (yamux) ===
  [server] stream echoed 4194304 bytes   × 10
  streams:           10 concurrent, 8 rounds each, 512 KiB payload
  total transferred: 80.0 MiB (both directions)
  wall clock:        19–26ms
  aggregate:         3.0–4.2 GiB/s
  slowest stream:    23ms
  isolation/order:   PASS (all 10 streams byte-exact)
```

**结果（smux）：** 同样 PASS，2.8 GiB/s，26ms。

**结论：两个库都满足"顺序与隔离"。**

### 4.2 半关闭语义（已实测）

```
=== half-close semantics (yamux) ===
  CloseWrite() available: no -> using Close()
  read after half-close: "RESPONSE-AFTER-EOF" (err=<nil>)
  server saw:            "REQUEST"

=== half-close semantics (smux) ===
  CloseWrite() available: yes
  read after half-close: "RESPONSE-AFTER-EOF" (err=<nil>)
  server saw:            "REQUEST"
```

**两者都能正确半关闭。** yamux 用 `Close()`，语义是"我只关写侧"；smux 有显式 `CloseWrite()`。**对端口转发这是必需能力**：TCP 代理必须能把一端的 FIN 传给另一端，同时继续反方向传数据。如果不支持半关闭，`io.Copy` 两个方向都结束后才能关连接，会导致 `HTTP/1.0` 这类"客户端先关写、等响应"的协议挂住。

### 4.3 流控隔离：一条流卡死是否影响其他流（已实测）

这是端口转发最关键的性质。测试：一条流（label `STAL`）的对端**永不读**，另一条（`ECHO`）正常回环。测量"卡死流"吸收多少字节后才阻塞，以及"健康流"此时还能跑多快。

```
yamux  stalled stream: 196608 bytes accepted before blocking (blocked=true)
       healthy stream: 100 round trips in 7-10ms = 1251-1680 MiB/s (ok=true)
       verdict: PASS - per-stream flow control, no cross-stream head-of-line blocking

smux1  stalled stream: 67108864 bytes accepted before blocking (blocked=true)
       healthy stream: 100 round trips in 9ms = 1322.9 MiB/s (ok=true)
       verdict: PASS
smux2  stalled stream: 65536 bytes accepted before blocking (blocked=true)
       healthy stream: 100 round trips in 12ms = 1049.7 MiB/s (ok=true)
       verdict: PASS
```

**yamux 在 192 KiB 处精确阻塞**（= 256 KiB 初始窗口减去已消费部分），符合 per-stream 流控的预期。**smux v1 吸收了 64 MiB 才"阻塞"**——这暴露了 §1.3 的问题：它不是被流控阻塞，而是写侧根本没有窗口检查（`writeV1`），只是因为 socket 缓冲区满了才慢下来。

### 4.4 数据完整性：慢读者 + 大流量（已实测，决定性）

接收端停顿 2s 后开始读，发送端推 64 MiB，比对字节数与 SHA-256：

```
yamux  writer: 67108864 bytes accepted in 2.103s
       reader: 67108864 bytes, err=<nil>
       ==> OK: all bytes arrived intact

smux1  writer: 67108864 bytes accepted in 2.065s
       reader: 61865984 bytes, err=io: read/write on closed pipe
       ==> DATA LOSS: 5242880 bytes accepted by Write() never arrived

smux2  writer: 67108864 bytes accepted in 2.154s
       reader: 67108864 bytes, err=EOF
       ==> OK: all bytes arrived intact
```

**重复 15 次 smux v1，2 次失败**（丢 22.0 MiB、8.9 MiB）。**这是本次研究最重要的负面发现**：smux v1 会在 `Write()` 返回成功的情况下丢失数据，且不可预测。

### 4.5 内存（已实测）

接收端永不读，100 条流填满窗口：

```
yamux, 100 streams, window=256.0 KiB
  idle:                249.5 KiB total  (2.5 KiB / stream)
  receive buffers full: 50.3 MiB total   (514.8 KiB / stream)
  => budget ~514.8 KiB per active forwarded connection at this window

yamux, 100 streams, window=1.0 MiB
  idle:                245.2 KiB total  (2.5 KiB / stream)
  receive buffers full: 200.3 MiB total  (2.0 MiB / stream)
```

**每活跃流 ≈ 2 × 窗口**。这是规划内存预算的公式。

### 4.6 并发上限（已实测）

```
yamux   opened 20000 streams in 177-254ms
        heap delta: 33.6-33.7 MiB  =>  1.7 KiB per idle stream
        NumStreams reported by library: 20000
```

20000 条流稳定，无错误。`AcceptBacklog` 默认 256 只影响"未 Accept 的流"，不影响已建立的流数。

### 4.7 吞吐（已实测，loopback）

**纯 yamux / smux，128 MiB/流：**

```
yamux   streams=1     window=1.0 MiB      3211.1 MiB/s
yamux   streams=10    window=1.0 MiB      2109.5 MiB/s
yamux   streams=50    window=1.0 MiB      2771.0 MiB/s
smux    streams=1     window=1.0 MiB      1452.1 MiB/s
smux    streams=10    window=1.0 MiB      1701.7 MiB/s
smux    streams=50    window=1.0 MiB      1760.7 MiB/s
```

**贴近 cmesh 真实栈（yamux → 消息适配器 → 每消息 AES-256-GCM → TCP），64 MiB/流：**

```
yamux+AEAD streams=1    window=1.0 MiB     1957.7 MiB/s
yamux+AEAD streams=10   window=1.0 MiB     1962.8 MiB/s
yamux+AEAD streams=50   window=1.0 MiB     2748.3 MiB/s
```

**窗口敏感性（10 流，64 MiB/流）：**

```
window=256.0 KiB   2060.7 MiB/s
window=512.0 KiB   2395.0 MiB/s
window=1.0 MiB     2373.0 MiB/s
window=4.0 MiB     2407.3 MiB/s
```

**默认 256 KiB 窗口只比 1 MiB 慢约 13%**（在 loopback 上）。在真实广域网上，窗口对吞吐的影响会大得多（BDP 限制：256 KiB / 50 ms RTT ≈ 40 Mbit/s 上限）。**推断**：跨公网部署时应把 `Window` 提到 1 MiB，代价是每活跃流转发占 2 MiB。

**QUIC 对照（32 MiB/流）：**

```
quic    streams=1       193.2 MiB/s
quic    streams=10      139.0 MiB/s
quic    streams=50      155.2 MiB/s
```

### 4.8 延迟（已实测，loopback，2000 次往返）

```
raw TCP          p50=15µs   p95=39µs   p99=50µs   mean=21µs
yamux stream     p50=18µs   p95=29µs   p99=60µs   mean=20µs
yamux (9 busy)   p50=191µs  p95=294µs  p99=345µs  mean=195µs
```

**空闲时 yamux 的延迟与裸 TCP 无差别**（+3 µs，在噪声内）。**但 9 条流跑满带宽时，p50 从 18 µs 涨到 191 µs（约 10 倍）**——这是单 `sendLoop` 串行化 + 批量调度的代价。对端口转发（HTTP/DB 这类请求-响应）在满载时会有可感知的延迟增加，但 191 µs 的绝对值仍然可接受。

### 4.9 传输消息数（已实测）

64 MiB 单流，统计底层 `Write` 调用次数（等价于 relay 转发的 WebSocket 消息数）：

```
per-Write  sent 64.0 MiB in 23ms  transport messages=801  avg msg=81.8 KiB  msgs/MiB=13  2789 MiB/s
```

**约 13 条消息/MiB，平均 81.8 KiB/条。** 相比现状（cmesh 用 512 KiB 帧，约 2 条/MiB），消息数增加约 6 倍。这会给 relay 的 splice 增加一些负担，但 relay 本来就是"哑管道"（`internal/relay/relay.go:1-24`），且 81.8 KiB/条仍在 `wsconn` 的 16 MiB `SetReadLimit` 之内（`wsconn.go:140`）。

**推断**：如果 relay 侧成为瓶颈，可以在适配器里做 coalescing（把 yamux 的 header+body 两次 Write 合并成一条消息）。原型中试过，但因实现 bug（flush 时机）未跑通，**本次没有可用数据**；需要时再单独验证。

---

## 5. 成本估算

### 5.1 依赖体积（已实测）

| 项 | yamux |
|---|---|
| 模块源码大小 | 164 KB |
| 间接依赖 | **0** |
| `go.sum` 新增行数 | 2 |
| 二进制增量（`-ldflags="-s -w"`，含 net.Listen 基线） | 2,204,722 → 2,238,418 = **+33,696 B ≈ 33 KB** |
| 许可证 | MPL-2.0（**文件级 copyleft**：修改 yamux 自身源码需开源该文件；cmesh 只是 import，不受影响） |

对比 smux：508 KB 源码，MIT，同样零依赖。

**许可证注意**：cmesh 是 AGPL-3.0-or-later。MPL-2.0 与 AGPL 兼容（MPL 是文件级 copyleft，AGPL 是项目级），只要不修改 yamux 源码就无需额外义务。**推断**：若将来需要 patch yamux，应把 patch 以 MPL 单独发布，而不是并进 AGPL 代码库。

### 5.2 代码量估算（推断，基于原型实测）

| 模块 | 非测试行数 | 测试行数 | 说明 |
|---|---|---|---|
| `internal/mux`（adapter + session + 流数限制） | ~200 | ~250 | 原型 `msgConn` 约 60 行，session 包装约 140 行 |
| `internal/forward`（preamble + AllowList + 双向 pipe + Limits） | ~350 | ~400 | 原型约 330 行（含 demo 驱动），AllowList 约 80 行 |
| `internal/cli/forward.go`（参数解析 + 生命周期） | ~250 | ~150 | `-L`/`-R` 解析是测试重点 |
| `peer.Session` 集成（Negotiator 顺序、消息接口） | ~80 | ~100 | 主要是把 mux 插进 `dialOne` |
| **合计** | **~880** | **~900** | 约 **2–3 个工程日** |

**为什么这么少：** 复用、流控、半关闭、keepalive 全部由 yamux 提供；cmesh 只需要写"适配 + 策略 + CLI"。原型已证明核心路径（§4.1、§3.4）可用。

### 5.3 与 `p2p2` / `p2pquic` 的关系

**`p2p2` 是死代码（已查证）。** `grep -rln "internal/p2p2"` 除自身目录外**零命中**，`internal/cli`、`internal/p2p`、`cmd/` 都不导入它。它有 4,700+ 行（含测试），自带 Reno 拥塞控制、SACK、批量收发。

**建议：不要为了端口转发去动 `p2p2`。** 它解决的是"UDP 上的可靠传输"，与"应用层多路复用"是正交问题。而且它的包注释明确写着 `Multiplexing ... are out of scope`（`p2p2/conn.go:26-30`）——设计上就没打算做复用。

**`p2pquic` 的关系：**

- **不需要重构。** 它作为"直连时的消息传输"继续工作，mux 插在它**之上**（§2.2）。
- **它的单流设计是对的。** `conn.go:68-80` 的注释解释了为什么不开多流：`AcceptStream` 不保序，而 `peer.Session` 要求严格顺序。引入 mux 后，这个约束由 mux 承担（每条流内部有序，流间无序），所以 `p2pquic` 保持单流即可。
- **一个潜在改进（可选）**：`p2pquic` 现在每条消息开一次 `Write`（`conn.go:235-240`，两次 Write：长度 + body）。如果 mux 在其上，消息数会增加。可以合并成一次 `Write`，减少系统调用。

**`p2p`（legacy）的关系：** 同理，不需要动。它只在 `--p2p-legacy` 时启用，且已知在丢包下会丢消息（`cli.go:134-136` 注释）。

### 5.4 一个必须澄清的架构问题：relay 是否会看到 mux 帧

**不会（已查证）。** relay 只做 WebSocket 消息的盲转发（`relay.go:1-24`："The relay's only job is to introduce two peers ... then splice their message streams. It cannot read anything"）。mux 帧在 `secure` 之后、`wsconn` 之前，所以 relay 看到的是**加密后的字节**，与现在完全一致。

**但有一个副作用（推断）**：mux 会增加消息数（§4.9，约 6 倍），relay 的 splice 循环次数随之增加。现有 relay 有 `MaxMessageBytes`（`relay.go:44`）和限流（`limiter.go`），需要确认这些限额对"更多但更小的消息"仍然合适。

---

## 6. 实施建议

### 6.1 分阶段

| 阶段 | 内容 | 可验证的产出 |
|---|---|---|
| P0 | `internal/mux`（adapter + yamux 包装 + 流数上限），插进 `dialOne` | 单测：10 流隔离、半关闭、流数上限触发 |
| P1 | `internal/forward`（preamble + AllowList + pipe），仅 `-L` | e2e：本地 8080 → 对端 echo 服务，10/10 通过 |
| P2 | `-R` + `--accept` + 默认拒绝 | e2e：白名单拒绝、`StatusDenied` 可见 |
| P3 | CLI 打磨（多 `-L`/`-R`、`--listen-all` 警告、`--expect-sas` 强制） | 参数表驱动测试 |
| P4 | 文档 + 安全说明（`docs/decisions.md` 新增一节） | — |

### 6.2 必须在 P0 就做对的四件事

1. **`yamux.Config.LogOutput = io.Discard`（或接 slog）。** 默认 `os.Stderr` 会污染 CLI 输出，且 `VerifyConfig` 要求 `LogOutput`/`Logger` 二选一非空。
2. **`MaxStreams` 用信号量在 `Open` 和 `Accept` 两侧都限制。** yamux 本身不限制已建立流数（只有 `AcceptBacklog` 限未 Accept 的）。不限制 = 对端可以开无限流耗尽内存。
3. **`CloseWrite()` 映射到 yamux 的 `Close()`，并记录状态。** 直接调两次 `Close()` 是安全的（`stream.go:324-378` 对 `streamClosed` 状态直接返回 nil），但 `Write` 之后的行为需要测试固定下来。
4. **`AllowList` 零值必须拒绝一切。** 不要用 `nil` 表示"允许全部"——这是开放代理的经典来源。

### 6.3 不建议做的事

- ❌ **不要给 `secure` 加 stream 概念。** `wire.Frame` 里那个 `Stream uint32` 字段（`frame.go:110`）目前**完全未被使用**（`grep -rn "Stream:"` 只有 3 处，全是测试与编解码本身）。**不要**试图用它做复用——那要求 secure 从"严格顺序的帧通道"变成"可按流重排的通道"，会破坏它的防重放语义（`secure.go:399-417` 依赖顺序）。让 yamux 在下面做复用，这个字段继续空着。
- ❌ **不要为转发做 stream 级断线续传。** TCP 连接是有状态的，跨 session 恢复一条半开的 TCP 流需要缓存全部未确认数据，复杂度远超收益。
- ❌ **不要在 `-R` 模式下让远端在 preamble 里指定任意目标。** 目标必须来自本地 `-R` 配置（§3.4 第 3 点）。

---

## 7. 风险清单

| 风险 | 严重度 | 已实测/推断 | 缓解 |
|---|---|---|---|
| **开放代理**：拿到 code 的人可访问容器可达的一切 | **高** | 已实测（白名单拒绝生效） | 默认 `--accept=false`；零值 AllowList 拒绝一切；`-R` 默认绑 loopback；强制 `--expect-sas` |
| 内存耗尽：对端开大量流 | 中 | 已实测（515 KiB/活跃流） | `MaxStreams` 信号量（默认 256）+ 每流窗口上限 |
| 重连导致全部转发断开 | 中 | 查证（`node.go:136-246` 重建 session） | 本地 listener 只建一次；断开时关闭所有转发连接；不做 stream 续传 |
| 单 `sendLoop` 在满载时增加延迟 | 低 | 已实测（p50 18 µs → 191 µs） | 可接受；必要时降 `Window` 或分多条底层连接 |
| yamux 版本仍是 v0.x，API 可能变 | 低 | 查证（v0.1.2） | 适配器隔离（`internal/mux` 只暴露自己的类型）；协议 spec 稳定 |
| `StreamOpenTimeout` 失败会关整个 session | 低 | 查证（`session.go:213-230`） | 设长一些（如 120s）；或在 `Open` 前先检查 `NumStreams` |
| relay 消息数增加约 6 倍 | 低 | 已实测（13 条/MiB） | 观察 relay CPU；必要时在适配器做 coalescing |
| MPL-2.0 与 AGPL 的组合 | 低 | 查证 | 不修改 yamux 源码即可；若需 patch 则单独发布 |

---

## 8. 附录：最小验证代码

完整可运行代码在 `/tmp/cmuxlab/`（独立 module，未入库）：

```
/tmp/cmuxlab/
├── go.mod, go.sum
├── muxlab/main.go        # demo / bench / limit / halfclose
├── flow/main.go          # 流控隔离（带 label 的精确版）
├── droptest/main.go      # 数据完整性（决定性）
├── memperstream/main.go  # 每流内存
├── memgrow/main.go       # 接收端内存增长
├── starve/main.go        # 缓冲区饥饿
├── hoc/main.go           # 队头阻塞
├── lat/main.go           # 延迟
├── msgcount/main.go      # 传输消息数
├── realistic/main.go     # yamux + 消息适配器 + AES-GCM（贴近 cmesh）
├── quicmux/main.go       # QUIC 原生多流对照
└── forward/main.go       # ssh -L 端到端原型 + 白名单
```

**复现命令：**

```bash
cd /tmp/cmuxlab
go build -o bin-demo ./muxlab   && ./bin-demo -cmd=demo -kind=yamux
go build -o bin-flow ./flow     && ./bin-flow -kind=yamux
go build -o bin-drop ./droptest && ./bin-drop -kind=yamux -bytes=$((64<<20)) -stall=2s
go build -o bin-mem  ./memperstream && ./bin-mem -streams=100 -window=$((256<<10))
go build -o bin-real ./realistic && ./bin-real -streams=10 -bytes=$((64<<20))
go build -o bin-fwd  ./forward  && ./bin-fwd
```

**关键片段 1：消息适配器（yamux 需要字节流，cmesh 的传输是消息型）**

```go
type messageStream struct {
	conn   peer.MessageConn // Send([]byte) error; Recv() ([]byte, error)
	remain []byte
}

func (m *messageStream) Read(p []byte) (int, error) {
	for len(m.remain) == 0 {
		b, err := m.conn.Recv()
		if err != nil {
			return 0, err
		}
		m.remain = b
	}
	n := copy(p, m.remain)
	m.remain = m.remain[n:]
	return n, nil
}

func (m *messageStream) Write(p []byte) (int, error) {
	// yamux calls Write from a single sendLoop, so no lock is needed here.
	// Copy because the caller may reuse p immediately after Write returns.
	buf := make([]byte, len(p))
	copy(buf, p)
	if err := m.conn.Send(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}
```

**关键片段 2：转发流的前置头与状态码**

```go
func writePreamble(w io.Writer, p Preamble) error {
	if len(p.Addr) > 255 {
		return errors.New("address too long")
	}
	buf := make([]byte, 2+len(p.Addr))
	buf[0] = p.Kind
	buf[1] = byte(len(p.Addr))
	copy(buf[2:], p.Addr)
	_, err := w.Write(buf)
	return err
}

// 接受端：先查策略，再拨号，最后回状态码。
// 顺序很重要：策略检查必须在 dial 之前，否则拒绝的请求也会产生一次
// 出站连接（可被用来探测/放大）。
if err := allow.Permits(p.Addr); err != nil {
	_ = binary.Write(st, binary.BigEndian, uint32(StatusDenied))
	return
}
target, err := net.DialTimeout("tcp", p.Addr, 5*time.Second)
if err != nil {
	_ = binary.Write(st, binary.BigEndian, uint32(StatusDialFail))
	return
}
defer target.Close()
_ = binary.Write(st, binary.BigEndian, uint32(StatusOK))
pipe(st, target)
```

**关键片段 3：零值即拒绝的白名单**

```go
func (a AllowList) Permits(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad target %q: %w", addr, err)
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return fmt.Errorf("bad port in %q", addr)
	}
	if len(a.Ports) > 0 && !slices.Contains(a.Ports, port) {
		return fmt.Errorf("port %d not in allow-list %v", port, a.Ports)
	}
	// An empty host list permits NOTHING. The zero value must be the safe
	// value: a forgotten configuration would otherwise be an open proxy.
	if len(a.Hosts) == 0 {
		return errors.New("empty allow-list: refusing every target")
	}
	ip := net.ParseIP(host)
	for _, h := range a.Hosts {
		if _, cidr, err := net.ParseCIDR(h); err == nil && ip != nil && cidr.Contains(ip) {
			return nil
		}
		if strings.EqualFold(h, host) {
			return nil
		}
		if strings.EqualFold(h, "localhost") && ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("target %q not in allow-list %v", addr, a.Hosts)
}
```

**端到端原型实测输出：**

```
[21:56:23.278] echo target listening on 127.0.0.1:19090
[21:56:23.278] tunnel up over a single TCP connection to 127.0.0.1:55030
[21:56:23.278] ssh -L equivalent: 127.0.0.1:18080 -> (tunnel) -> 127.0.0.1:19090
[21:56:23.279] stream established to 127.0.0.1:19090    × 10
[21:56:23.280] forward test: 10/10 connections round-tripped correctly in 2ms
[21:56:23.280] REFUSED 127.0.0.1:22: port 22 not in allow-list [19090]
[21:56:23.280] allow-list check: peer status=1 (1 = refused, expected)
```

---

## 9. 参考

- yamux 源码：`/Users/blackevil/go/pkg/mod/github.com/hashicorp/yamux@v0.1.2/`（`spec.md` 是协议规范，`session.go:472-529` 是 sendLoop，`stream.go:324-378` 是 Close/半关闭）
- smux 源码：`/Users/blackevil/go/pkg/mod/github.com/xtaci/smux@v1.5.57/`（`stream.go:490-530` 是 `writeV1` 无窗口检查的证据，`stream.go:533-640` 是 v2 的对照）
- quic-go：`/Users/blackevil/go/pkg/mod/github.com/quic-go/quic-go@v0.55.0/`（`connection.go:2605-2650` 是 stream API，`internal/protocol/params.go:40` 是默认 100 条入站流上限）
- cmesh 相关文件：`internal/peer/session.go`（readPump/sendMu）、`internal/secure/secure.go`（计数器与顺序要求）、`internal/peer/switchable.go`（传输切换）、`internal/cli/node.go`（重连生命周期）、`internal/wire/frame.go:110`（未使用的 `Stream` 字段）
- pkg.go.dev: https://pkg.go.dev/github.com/hashicorp/yamux（v0.1.2，1,681 个导入者，MPL-2.0）
- 项目内既有决策：`docs/decisions.md` 的 `D38. SAS 强制比对`、`D43–D46`（QUIC 相关）、`D28. P2P 直连改为默认关闭`
