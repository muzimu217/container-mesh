# container-mesh 阶段 1：端口转发详细技术设计

- 版本：v1.0（设计稿，不含实现代码）
- 日期：2026-10-21
- 代码基线：`container-mesh@be15796`
- 上游文档：
  - [`ARCHITECTURE-SDWAN.md`](ARCHITECTURE-SDWAN.md)（总体架构，阶段 1 见 §4.3、§6）
  - [`../research/mux-and-portforward.md`](../research/mux-and-portforward.md)（yamux 选型、端口转发原型、开放代理防御）
- 证据标注沿用上游口径：**`[实测]`** = 研究或项目历史实际跑出的数字；**`[查证]`** = 读源码确认；**`[推断]`** = 工程判断，未实测
- 标注约定：**【必须】** = 不做就会出错或不安全；**【可选】** = 可以在后续迭代补

---

## 0. 摘要

本阶段交付 `cmesh forward -L/-R`：在一条已加密的 relay/QUIC 连接上，用 yamux 开出多条 TCP 流，把本地端口映射到对端可达的服务（SSH、MySQL、HTTP）。零特权、零 TUN、零设备创建。

**五个核心结论：**

1. **分层顺序是 `transport → secure → 记录泵 → yamux → 业务流`，研究二的「mux 在 secure 之下」不能照字面实现。** `secure.Session` 是每方向一个严格递增计数器、且要求帧严格按发送顺序到达的 AEAD 通道（`secure.go:399-417`）。yamux 的 `recvLoop`（`session.go:548`）在应用读取之前就经 `handleStreamMessage`（`session.go:578-630`）按 streamID 把帧分发到各流的缓冲区，跨 stream 的到达顺序在那一刻就丢了。若多个 goroutine 各自解密，它们会看到**乱序的全局计数器**，解密必然失败。记录泵把「多流并发」重新收敛成「单序列」，`secure.go` 一行不改。

2. **`wire.Version` 不升到 2。** 帧格式（10 字节头、1 MiB 上限）完全不变，新增帧类型是纯增量；而 `exchangeHello` 在版本不一致时直接返回 `ErrProtocol`、没有降级路径（`session.go:244-246`），升版本会让**现有 `send`/`recv` 在新旧二进制之间直接不可用**——这是纯粹的功能倒退。改用 hello 里的 `caps` 能力位协商，旧对端自动退化为「不支持转发」，文件传输完全不受影响。这是对 ARCHITECTURE-SDWAN §A1 的修正（理由见 §3.4）。

3. **记录大小取 32 KiB，一条记录一次 transport 写（不合并）**（本项经评审后两次修正：
   原方案 512 KiB，评审后改 32 KiB + 16 条批量，实现时实测又否掉了批量）。

   最初的推理是「512 KiB 比 64 KiB 快 2.5 倍（539.3 vs 211.9 MiB/s），所以
   取 512 KiB」。这个推理错在**优化目标选错了**：那两个数字都是回环测出来的，
   而真实链路是 2–12 MiB/s，AEAD 从来不是瓶颈（实测 32 KiB 记录仍有
   **1408 MB/s**）。

   真正被牺牲的是**延迟**。记录泵必须收齐整条记录、通过 AEAD 认证，才能交给
   yamux 分发；在 2 MiB/s 链路上，一条 512 KiB 记录要传 **250 ms**，这期间
   同一连接上的其他流全部等待：

   | 记录大小 | 2 MiB/s 链路上的队头等待 | 回环固定开销占比 |
   |---|---|---|
   | 16 KiB | 7.8 ms | 6.4% |
   | **32 KiB** | **15.6 ms** | **3.2%** |
   | 64 KiB | 31.3 ms | 1.6% |
   | 512 KiB | **250 ms** | 0.2% `[实测]` |

   一边 `scp` 传大文件、一边在同一隧道里敲 SSH，512 KiB 会让终端明显卡顿。

   **批量合并（16 条拼成一次 transport 写）在实现阶段被否掉了。** 理由是它把
   刚砍掉的队头阻塞原封不动装回来：接收端必须收齐整条 AEAD 消息才能解密，所以
   16 × 32 KiB = **512 KiB 一条**，在 2 MiB/s 链路上又是 250 ms；而且它恰好只在
   「链路正忙」时才触发，正是交互流量最需要低延迟的时刻。

   代价是 syscall 次数：512 MiB 从 1024 次写变成 16384 次。实测表明这个代价可以
   接受——`[实测]` 512 MiB 回环三次为 **515 / 615 / 594 MiB/s**，全部高于 485
   MiB/s 的回归门槛。如果将来 syscall 真的成为瓶颈，优化应该落在传输层
   （sendmmsg 式批量），而不是加大加密单元。

   这条与 TLS 1.3 把 record 上限定在 16 KiB 是同一个道理：**加密单元的大小就是
   延迟下限，不能为了省 syscall 而放大它。**

4. **`-R` 比 `-L` 危险一个量级，需要独立的授权开关和端口白名单。** `-L` 是「我允许你拨我配置好的目标」，`-R` 是「我允许你在我这里开一个监听端口」。后者可以让持有 code 的攻击者在受害容器里绑定任意空闲 loopback 端口、向本地进程提供内容（例如冒充一个本地开发服务器）。研究二 §3.4 的 5 层里只有一层（第 1 层）管「是否启用」，且正文写 `--allow-forward`、参数表写 `--accept`，本身不一致；`-R` 没有任何独立授权。本设计增加 `--accept-reverse` + `--allow-listen-port`（零值拒绝一切）。

5. **白名单必须「先解析成 IP、再比对、再用同一个 IP 拨号」。** 研究二原型的 `Permits(addr)`（§8 关键片段 3，第 974-1004 行）拿主机名做字符串比对，随后 `net.DialTimeout("tcp", p.Addr, ...)`（第 961 行）再解析一次——这是典型的 check-then-dial TOCTOU，DNS rebinding 可以绕过。本设计把 `Permits` 的返回值定义为**要拨的具体 IP**，拨号只用它。

**工作量**：初版估 2,575 行生产 + 2,750 行测试。评审认为对阶段 1 偏重，据此砍掉了
重复包装与冗余生命周期，并把完整 ACL 推到 PR #3，总量降到
**2,275 行生产 + 2,460 行测试**，拆成 3 个可独立合并的 PR（§7）。
与评审估的 800–1,200 行仍有差距，逐项差额见 §7.1——结论是那三处（记录泵的双模式
与屏障、mux 的内存预算与日志接管、`peer.Session` 的结构改造）都不是抽象层，
而是现有功能不被破坏的前提。

---

## 1. 包结构与文件划分

### 1.1 依赖方向（无环）

```
cmd/cm
 └── internal/cli          forward.go（新）；node.go（改 ~60 行）
      ├── internal/forward   策略 + 监听器 + 双向 pipe
      │     ├── internal/mux     yamux 包装（只依赖 yamux + stdlib）
      │     └── internal/wire    前置头/状态码/控制帧（纯编解码）
      ├── internal/peer       session.go（改 ~90 行）、pump.go（新）、frameconn.go（新）
      │     ├── internal/secure   【不改】
      │     └── internal/wire
      └── internal/wsconn / p2pquic / p2p   【不改】
```

关键约束：`internal/mux` **不导入** `wire`/`peer`/`secure`。它只认识「一条有序字节流」和「一个字节的 stream kind」，因此 yamux 的 v0.x API 变动被完全隔离在这一个包里（研究二 §7 的风险缓解）。

### 1.2 新建文件

| 文件 | 职责 | 预计行数（生产/测试） |
|---|---|---|
| **`internal/wire/forward.go`** | 转发子协议的全部线格式：`Preamble` 编解码、`Status` 编解码、`ForwardListenSpec/Status` 结构、状态码常量。放在 `wire` 而不是 `forward`：否则 `forward` 定义状态码、`wire` 定义帧 payload 会形成导入环 | 170 / 200 |
| **`internal/wire/frame.go`**（改） | 新增 4 个 `FrameType` 与其 `String()`；`MaxStreamData` 常量 | +35 / +20 |
| **`internal/peer/pump.go`** | **记录泵**。唯一持有 `*secure.Session` 的组件；引导模式直通帧，复用模式把字节流封成 `FrameStreamData` | 280 / 320 |
| **`internal/peer/frameconn.go`** | `FrameConn` 接口 + `streamFrameConn`（在 yamux 控制流上收发 `wire.Frame`）+ `writeFull` | 130 / 160 |
| **`internal/peer/session.go`**（改） | `conn`+`sec` 两个字段换成 `frames FrameConn` + `pump *Pump`；新增 `StartMuxed`/`AttachControl` | 改 ~90 / +120 |
| **`internal/mux/config.go`** | `Config` + 默认值 + 内存预算换算（窗口 × 流数 ≤ 预算） | 80 / 90 |
| **`internal/mux/mux.go`** | `Session` 包装：`New`、`Open`、`Serve`（单 accept 循环 + kind 路由）、流数信号量、`Close` | 200 / 220 |
| **`internal/mux/stream.go`** | `Stream`：`Read/Write/CloseWrite/Close/SetDeadline`、`Kind`、`Limited`。信号量归还与 kind 表放在 `mux.go`，不在这里再包一层 | 70 / 100 |
| **`internal/forward/allow.go`** | `AllowList`：主机名启动时解析成 IP 集、CIDR、端口/端口段、`Permits` 返回要拨的 IP、拒绝 `0.0.0.0/0` | 130 / 180 |
| **`internal/forward/limits.go`** | 流数、**建流速率（手写令牌桶，约 40 行）**、空闲超时、会话寿命 | 60 / 70 |
| **`internal/forward/pipe.go`** | 双向 pipe + 半关闭传播 + 错误归一 | 100 / 130 |
| **`internal/forward/local.go`** | `-L`：本地 listener 只建一次、每连接开流、前置头+状态、挂到当前 session | 190 / 200 |
| **`internal/forward/reverse.go`** | `-R`：注册请求、托管对端请求的 listener、按 ID 匹配、本地拨号 | 200 / 220 |
| **`internal/forward/manager.go`** | `Manager`：装配、`Serve` 的 kind 分发、审计日志、启动横幅 | 150 / 170 |
| **`internal/cli/forward.go`** | `-L`/`-R` 解析（表驱动）、全部策略 flag、`--also-recv-files`、生命周期 | 300 / 200 |
| **`internal/cli/node.go`**（改） | `UseMux`、`OnMux` 钩子、`dialOne` 里插入 mux 引导、`Mux(ctx)` 访问器 | 改 ~60 / +60 |
| **`internal/cli/cli.go`**（改） | 子命令注册 + usage 文本 | +30 / — |

合计：**2,275 生产 / 2,460 测试**（上表逐项相加）。

> 初版是 2,575 / 2,750。评审后收紧了五处：`stream.go` 不再重复包一层（信号量与
> kind 表并入 `mux.go`，−60）、`allow.go` 收紧（−80）、`manager.go` 收紧（−80）、
> `cli/forward.go` 去掉冗余生命周期封装（−40）。**令牌桶保留但重写**（100 → 60，
> 理由见 §7.1）。剩下的每一行都对应一个可指名的验收项——见 §7 的分批依据与 §8
> 的逐步自证。

### 1.3 不新建、不修改的包

| 包 | 处置 | 理由 |
|---|---|---|
| `internal/secure` | **一行不改** | 记录泵的全部意义就在于让它继续只看到「一条有序的帧序列」。`SealFrame`/`OpenFrame` 的现有测试（重放/篡改/计数器）继续有效 |
| `internal/wsconn` / `p2pquic` / `p2p` | 不改 | 它们只提供 `MessageConn`，与 mux 正交 |
| `internal/p2p`（打洞） | 不改 | `Negotiator` 的时机天然在 mux 之前（`session.go:271-289`），满足 A8 |
| `internal/p2p2` | 不接入 | A12 冻结策略 |
| `internal/relay` | 不改 | relay 只转发密文；消息数会从约 2 条/MiB 升到约 4–13 条/MiB（研究二 §4.9 `[实测]`），仍在 `MaxMessageBytes=16 MiB` 与限流之内 |

---

## 2. 接口定义

### 2.1 记录泵（`internal/peer/pump.go`）

记录泵是**唯一接触 `secure.Session` 的组件**，也是 `MessageConn`（消息语义）与 yamux（字节流语义）之间的适配器。它不是研究二 §2.5 那个 `messageStream`——那个适配器位于 secure **之下**，会让 secure 看到多流交错的密文。

```go
package peer

// Pump owns the encrypted frame channel. It has exactly two modes:
//
//   bootstrap: SendFrame/RecvFrame are the only entry points, and they are
//     byte-for-byte what peer.Session did before the mux existed. hello and
//     the p2p signalling run here.
//
//   muxed: StartMux has completed; one goroutine reads the transport and one
//     writes it, and the pair presents a single ordered byte stream to yamux.
//
// The invariant that makes the whole design work: exactly one goroutine ever
// calls secure.OpenFrame, and exactly one ever calls secure.SealFrame. That is
// what the strictly increasing counter in secure requires, and it is why
// multiplexing cannot sit below secure.
type Pump struct {
    conn MessageConn      // the *switchable, so a p2p upgrade still works
    sec  *secure.Session

    // bootstrap-side serialisation. After StartMux these are unused: the
    // writer goroutine is then the only seal path.
    bootMu sync.Mutex

    st      atomic.Int32 // pumpBootstrap -> pumpDraining -> pumpMuxed -> pumpClosed
    rwc     *pumpLink    // non-nil once muxed
    readQ   *byteQueue   // inbound records, bounded (see BufBytes)
    writeQ  *byteQueue   // outbound records, bounded

    closeOnce sync.Once
    closeErr  error
    done      chan struct{}
}

type PumpConfig struct {
    // RecordSize is the largest FrameStreamData payload.
    //
    // 32 KiB, not 512 KiB. The 512 KiB figure came from loopback throughput
    // (D11: 539.3 vs 211.9 MiB/s) and is the wrong optimisation target here:
    // AEAD is not the bottleneck on any real link (measured: 1408 MB/s at
    // 32 KiB), while a large record is a latency floor for every other stream
    // sharing the connection. A 512 KiB record takes 250 ms to cross a
    // 2 MiB/s link, and nothing can be decrypted or dispatched until the whole
    // record arrives, so an interactive stream waits behind it. At 32 KiB that
    // floor is 15.6 ms.
    //
    // The write side sends one record per transport message. Batching was
    // designed in and then removed: concatenating N records means the receiver
    // cannot decrypt any of them until all N arrive, which puts the 250 ms
    // head-of-line floor back on exactly the busy links where the low floor
    // matters most. Measured cost of per-record sends: 515-615 MiB/s on
    // loopback for 512 MiB, above the 485 MiB/s regression gate.
    RecordSize int
    // BufBytes bounds each direction's queue. 1 MiB is 32 records at the
    // default size: enough to keep the transport busy across one AEAD + one
    // Send, small enough that a dead peer cannot pin more than 2 MiB.
    BufBytes int
    Logger   *slog.Logger
}

func NewPump(conn MessageConn, sec *secure.Session, cfg PumpConfig) *Pump

// ---- bootstrap mode -------------------------------------------------------

// SendFrame seals and sends one frame. Safe for concurrent use, though during
// bootstrap only one goroutine (hello, then negotiation) is ever writing.
func (p *Pump) SendFrame(f *wire.Frame) error

// RecvFrame reads and opens one frame. Only the caller that owns the read side
// may call it: exchangeHello, then p2p.Negotiate, then nobody.
func (p *Pump) RecvFrame() (*wire.Frame, error)

// ---- barrier --------------------------------------------------------------

// StartMux performs the FrameMuxStart barrier and hands back the byte stream
// yamux runs on.
//
// Sequence (both peers run it symmetrically, right after negotiation):
//
//   1. seal+send FrameMuxStart
//   2. read frames until the peer's FrameMuxStart arrives; anything else is
//      logged and dropped (it can only be a stray pong: the read pump has not
//      started and the application cannot be writing yet)
//   3. start the reader goroutine (transport.Recv + OpenFrame) and the writer
//      goroutine (SealFrame + transport.Send)
//   4. return the link
//
// Steps 1-2 run on the caller's goroutine, so the "one reader" invariant holds
// across the hand-off: before StartMux the caller reads, after it the pump
// goroutine reads, and there is no moment when two readers exist.
func (p *Pump) StartMux(ctx context.Context) (io.ReadWriteCloser, error)

// ---- lifecycle / accessors ------------------------------------------------

// Close shuts the pump down. Closing the link also closes yamux (its recvLoop
// and sendLoop both error out), which fails every stream.
func (p *Pump) Close() error
func (p *Pump) Done() <-chan struct{}
func (p *Pump) Err() error

// Delegated accessors so peer.Session and cli.Node keep their current API.
func (p *Pump) ShortAuth() string
func (p *Pump) Fingerprint() string
func (p *Pump) RecvCounter() uint64

// pumpLink is what yamux sees: an ordered, reliable, closeable byte stream.
type pumpLink struct{ p *Pump }

func (l *pumpLink) Read(b []byte) (int, error)   // drains p.readQ
func (l *pumpLink) Write(b []byte) (int, error)  // appends to p.writeQ
func (l *pumpLink) Close() error                 // p.Close()
```

**核心循环（伪代码，实现者按此写）：**

```go
// reader — the only goroutine that touches transport.Recv and OpenFrame
func (p *Pump) readLoop() {
    defer p.fail(errPumpRead)
    for {
        msg, err := p.conn.Recv()
        if errors.Is(err, ErrTransportSwitched) {
            // A direct path replaced the relay underneath us. The session key
            // and the counters are unchanged, so keep going. Forgetting this
            // branch is the single most likely regression: it would turn a
            // successful --p2p upgrade into a dead session.
            continue
        }
        if err != nil { p.fail(err); return }

        f, err := p.sec.OpenFrame(msg)   // single reader => counters stay in order
        if err != nil { p.fail(err); return }

        switch f.Type {
        case wire.FrameStreamData:
            if err := p.readQ.Put(f.Payload); err != nil { p.fail(err); return }
        default:
            // Nothing legitimate travels outside FrameStreamData after the
            // barrier. Drop it rather than kill the session: a ping that was
            // in flight when the peer hit the barrier is harmless.
            p.log.Debug("dropping non-stream frame in mux mode", "type", f.Type)
        }
    }
}

// writer — the only goroutine that touches SealFrame and transport.Send
func (p *Pump) writeLoop() {
    defer p.fail(errPumpWrite)
    buf := make([]byte, p.cfg.RecordSize)
    for {
        n, err := p.writeQ.Get(buf)   // blocks; returns whatever is available
        if err != nil { p.fail(err); return }
        // One record = one frame = one AEAD = one transport message. The frame
        // header's Stream field stays 0: multiplexing belongs to yamux, and
        // wire.Frame.Stream must not become a second, conflicting stream id.
        f := &wire.Frame{Type: wire.FrameStreamData, Payload: buf[:n]}
        sealed, err := p.sec.SealFrame(f)
        if err != nil { p.fail(err); return }
        if err := p.conn.Send(sealed); err != nil { p.fail(err); return }
    }
}
```

**必须处理的四个细节：**

1. **`ErrTransportSwitched` 必须继续，但 mux 模式下禁止中途切换。**

   `readPump` 现在有这条分支（`session.go:307-313`），搬进泵时不能丢——丢了会让
   `--p2p` 打洞成功后瞬间判死会话。

   但**在 mux 模式下，这条分支必须改成「锁定当前路径」而不是「继续读」**。
   原因是记录泵会把字节流切成记录，而切换会丢弃旧连接上在途的字节：

   `[实测]` 若切换发生在一条记录的中途，已读到的半条记录永久损坏（测试读到
   24/49 字节后连接关闭，剩余 25 字节再也不会到达）。`secure.OpenFrame` 的下一个
   帧因此与计数器/字节边界都不对齐，必然解密失败，`fail()` 关掉两端，
   **所有 yamux 流同时断掉** —— 所谓热切换退化成「切换即断连」。

   阶段 1 的规则：

   | 模式 | 切换语义 |
   |---|---|
   | 单流（现有 `send`/`recv`） | 允许，行为不变（帧边界清晰，最多丢一帧，上层重传） |
   | **mux（`cmesh forward`）** | **建连后锁定路径**。P2P 打洞只在 `StartMuxed` 之前尝试；一旦 mux 跑起来，`Switch` 返回错误而不是替换连接 |

   这样代价明确：mux 会话的生命周期内路径不变。路径真的坏了，会话失败、上层重连
   （`Node.Run` 已经会重建 session），而不是假装能无损迁移。真正的无损迁移需要
   在记录泵之下加一层带序列号与重传缓冲的虚拟会话层，那是阶段 3 之后的事，不在
   阶段 1 的范围内。
2. **`fail` 必须同时关闭两个队列和 link。** 只关一端会让 yamux 的另一个 loop 永久阻塞，整个会话停摆（ARCHITECTURE-SDWAN §6 已列为风险）。
3. **队列是有界阻塞的，不是丢弃的。** 满时阻塞写侧，这是正确的背压（传输慢 → yamux `sendLoop` 慢 → 各流的 `Write` 慢 → 应用的 `write()` 慢），不是 bug。`BufBytes=1 MiB` 下，11–12 MiB/s 的跨公网链路约 85 ms 排空，远小于 yamux 的 `ConnectionWriteTimeout=10 s` 安全阀。
4. **`SealFrame` 成功但 `Send` 失败时计数器已经消耗。** 会话必须判死，不能重试——这与现有 `SendFrame` 的行为一致（`session.go:423-431`），不是新增缺陷。

**不做写入合并（coalescing）。** 设计初稿把「16 条合并成一次 transport 写」定为
必须，实现时发现它与本设计的核心目标冲突，已删除：

- **接收端必须收齐整条 AEAD 消息才能解密**，所以 16 条 32 KiB 记录合并后是一个
  **512 KiB 的加密单元**，在 2 MiB/s 链路上仍是 250 ms 的队头等待——正是缩小记录
  想消除的那个数字。
- **它只在链路忙时才触发**，也就是交互流量最需要低延迟的时候。空闲链路上队列里
  只有一条记录，合并根本不发生。所以「空闲时延迟低、繁忙时延迟高」恰好是反的。
- **收益只是 syscall 次数**，而实测表明这不是瓶颈：一条 32 KiB 记录已经把一次
  写摊销到 32 KiB 上，512 MiB 回环三次为 515 / 615 / 594 MiB/s。

如果将来 syscall 真的成为瓶颈，正确的做法是在**传输层**做批量（`sendmmsg` 式，
一次系统调用写多条消息），而不是放大加密单元。后者会同时放大延迟下限和丢包代价
（一条消息丢失 = 16 条记录重传）。

**不做基于延时的 flush** 这条结论不变：研究二试过并因 flush 时机出 bug 未跑通。

### 2.2 `FrameConn`：把 yamux 接进 `peer.Session` 的接缝

**这是整个改造的关键抽象。** `peer.Session` 现在同时持有传输（`conn *switchable`）和加密（`sec *secure.Session`）。改成只持有一个 `FrameConn`：

```go
package peer

// FrameConn is the session's frame source and sink. There are exactly two
// implementations, and which one is installed changes exactly once per
// session, at the mux barrier:
//
//   *Pump           bootstrap: transport messages + AEAD
//   streamFrameConn muxed:     wire frames on a yamux control stream
//
// peer.Session never learns which one it has. That is what keeps the file
// transfer code (offers, resume, hash verification, duplex) untouched.
type FrameConn interface {
    SendFrame(*wire.Frame) error
    RecvFrame() (*wire.Frame, error)
    Close() error
}
```

`streamFrameConn` 就是「消息语义 ↔ 流语义」在**控制流**上的适配器：

```go
// streamFrameConn carries wire frames on a byte stream.
//
// Note there is no extra length prefix: wire.Frame already encodes its own
// payload length (frame.go:118-130), so wire.DecodeFrame(io.Reader) reads
// exactly one frame. Adding a second length prefix (the "one Write = one
// message" adapter from research §2.5) would be pure overhead here.
type streamFrameConn struct {
    rw     io.ReadWriteCloser
    rd     *bufio.Reader   // 64 KiB: wire.DecodeFrame does two Reads per frame
    sendMu sync.Mutex      // one frame must not interleave with another
}

func newStreamFrameConn(rw io.ReadWriteCloser) *streamFrameConn {
    return &streamFrameConn{rw: rw, rd: bufio.NewReaderSize(rw, 64<<10)}
}

func (c *streamFrameConn) SendFrame(f *wire.Frame) error {
    b, err := f.Encode(nil)   // reuse the existing encoder: header + payload
    if err != nil { return err }
    c.sendMu.Lock()
    defer c.sendMu.Unlock()
    // writeFull, not Write: a short write would desynchronise the frame
    // boundary for the rest of the session.
    return writeFull(c.rw, b)
}

func (c *streamFrameConn) RecvFrame() (*wire.Frame, error) {
    return wire.DecodeFrame(c.rd)   // frame.go:133-154, unchanged
}

func (c *streamFrameConn) Close() error { return c.rw.Close() }
```

**为什么不用研究二 §2.5 的 `messageStream`：**

| | 研究二的 `messageStream` | 本设计的记录泵 |
|---|---|---|
| 位置 | secure **之下**（yamux 直接包在 `MessageConn` 上） | secure **之上**（yamux 包在泵提供的字节流上） |
| secure 看到什么 | 多流交错的密文，需要按 streamID 重排 | 一条有序的字节序列，计数器天然递增 |
| 能否满足 `OpenFrame` 的顺序要求 | 不能：`recvLoop` 在应用读取前就按流分发 | 能：单读者 |
| 每流密钥 | 必须派生（nonce 空间风险） | 不需要 |

`messageStream` 唯一还需要的场景是「把一条 yamux 流当成 `Send`/`Recv` 消息管道」。控制流不需要它（帧自带长度），转发流也不需要它（直接 `io.Copy`）。所以**不实现它**。

### 2.3 `peer.Session` 的改动

```go
type Session struct {
    cfg Config
    frames FrameConn   // was: conn *switchable + sec *secure.Session
    pump   *Pump       // nil only for sessions built by tests that inject frames
    // ... 其余字段不变
}

// StartMuxed is StartWithNegotiation with a mux bootstrap step inserted
// between the p2p negotiation and the read pump. The order is load-bearing:
//
//   hello -> negotiation (+ transport switch) -> mux barrier -> control stream -> read pump
//
// A8: the transport may only change before yamux exists. The read pump may
// only start once the frame source is the control stream, or it would race
// StartMux for the next frame on the raw transport.
func (s *Session) StartMuxed(h Handler, neg Negotiator, boot MuxBoot) error

// MuxBoot is supplied by the CLI. It performs the barrier, builds the yamux
// session, starts its accept loop, and returns the control stream.
type MuxBoot func(*Session) (io.ReadWriteCloser, error)

// AttachControl installs the control stream as the frame source. Called from
// the accept loop's kind=0 branch (responder side).
func (s *Session) AttachControl(rw io.ReadWriteCloser) error

// 既有访问器改为委托，node.go 无需改动：
func (s *Session) ShortAuth() string  { return s.pump.ShortAuth() }
func (s *Session) Fingerprint() string { return s.pump.Fingerprint() }
func (s *Session) UpgradeToDirect(next MessageConn) error { return s.pump.UpgradeToDirect(next) }
func (s *Session) OnDirect() bool { return s.pump.OnDirect() }
func (s *Session) FramesSinceStart() uint64 { /* pump.RecvCounter() - recvAtStart */ }
```

`SendFrame`/`recvFrame` 变成两行：

```go
func (s *Session) SendFrame(f *wire.Frame) error { return s.frames.SendFrame(f) }
func (s *Session) recvFrame() (*wire.Frame, error) { return s.frames.RecvFrame() }
```

原来的 `sendMu`（「密封 + 发送必须原子」，`session.go:416-431`）职责上移到两个实现里：`Pump.SendFrame` 用 `bootMu`，`streamFrameConn.SendFrame` 用 `sendMu`。**必须**保留这个语义，否则并发 `SealFrame` 会让高计数器先上线。

### 2.4 `internal/mux`

```go
package mux

// Kind is the first byte of every stream. It tells the accept loop what the
// stream is for before any protocol bytes are read.
const (
    KindControl byte = 0 // the peer.Session control stream (opener: initiator)
    KindForward byte = 1 // one forwarded TCP connection
)

type Role int
const (
    RoleClient Role = iota // yamux client: opens odd stream ids (role A)
    RoleServer             // yamux server: opens even stream ids (role B)
)

type Config struct {
    Role Role
    // Window is yamux MaxStreamWindowSize. 256 KiB is the yamux floor and the
    // memory-safe default (2*Window per active stream, measured 514.8 KiB).
    // 1 MiB is worth ~13% on loopback but is required on WAN: 256 KiB over a
    // 50 ms RTT caps a stream at ~40 Mbit/s.
    Window uint32
    // MaxStreams bounds concurrently open streams on both sides.
    MaxStreams int
    // AcceptBacklog must be >= MaxStreams, or a burst of inbound streams is
    // RST by yamux before the accept loop can drain it.
    AcceptBacklog int
    // StreamOpenTimeout: yamux closes the WHOLE session when one stream's SYN
    // is not acknowledged in time (session.go:213-230). 75 s is too tight when
    // a relay hiccups; 120 s costs nothing.
    StreamOpenTimeout time.Duration
    // StreamCloseTimeout: how long a half-closed stream may linger. The yamux
    // default of 5 min would hold 2*Window for 5 min per abandoned forward.
    StreamCloseTimeout time.Duration
    ConnectionWriteTimeout time.Duration
    KeepAliveInterval      time.Duration
    Logger *slog.Logger
}

type Session struct {
    y   *yamux.Session
    cfg Config
    sem chan struct{}  // MaxStreams, shared by Open and Accept
    log *slog.Logger
}

func New(rwc io.ReadWriteCloser, cfg Config) (*Session, error)

// Open opens one stream and writes its kind byte. It blocks while the stream
// limit is reached, honouring ctx. Checking NumStreams first avoids the case
// where yamux's own StreamOpenTimeout fires while we are waiting for a slot.
func (s *Session) Open(ctx context.Context, kind byte) (*Stream, error)

// Serve runs the single accept loop. Each accepted stream is dispatched in its
// own goroutine. When the limit is reached the stream is still accepted and
// marked Limited: refusing to accept makes yamux RST it, and the opener would
// see a reset instead of an explicit "too many" status.
func (s *Session) Serve(ctx context.Context, dispatch func(*Stream)) error

func (s *Session) NumStreams() int
func (s *Session) Ping() (time.Duration, error)
func (s *Session) Close() error
func (s *Session) Done() <-chan struct{}

type Stream struct {
    st      *yamux.Stream
    kind    byte
    limited bool
    sem     chan struct{}
    once    sync.Once
}

func (s *Stream) Read([]byte) (int, error)
func (s *Stream) Write([]byte) (int, error)

// CloseWrite half-closes the write side. yamux has no CloseWrite, but
// Stream.Close is exactly a FIN: stream.go:324-379 only sets
// streamLocalClose and sends FIN, and Read keeps working (stream.go:100-106).
// Measured: an 18-byte response was still readable after Close.
func (s *Stream) CloseWrite() error
func (s *Stream) Close() error      // releases the semaphore exactly once
func (s *Stream) SetDeadline(t time.Time) error
func (s *Stream) Kind() byte
func (s *Stream) Limited() bool
func (s *Stream) ID() uint32
```

**yamux 参数取值与理由：**

| 参数 | yamux 默认 | 本设计 | 为什么 |
|---|---|---|---|
| `MaxStreamWindowSize` | 256 KiB | 256 KiB（`--mux-window` 可调） | 每活跃流 ≈ 2×窗口 `[实测]`：256 KiB → 514.8 KiB，1 MiB → 2.0 MiB。默认取内存安全值 |
| `MaxStreams` | 无上限 | 256，且被预算硬压 | yamux 只限「未 Accept 的流」（`AcceptBacklog`），已建立流数没有上限。不设限 = 对端可开无限流耗尽内存 |
| `AcceptBacklog` | 256 | `max(256, MaxStreams)` | 小于 MaxStreams 时突发会被 RST |
| `StreamOpenTimeout` | 75 s | **120 s** | 失败会关整个 session（`session.go:213-228` `[查证]`） |
| `StreamCloseTimeout` | 5 min | **60 s** | 半关闭后等对端 FIN 的上限；5 分钟太长，一条废弃转发会占 2×窗口 5 分钟 |
| `ConnectionWriteTimeout` | 10 s | 10 s | 安全阀：写不出去就认为连接坏了。跨公网 11–12 MiB/s 下 1 MiB 队列 85 ms 排空，10 s 足够 |
| `KeepAliveInterval` | 30 s | 30 s | 与 wsconn 的 20 s ping 重叠但无害；yamux 的 keepalive 失败会主动关 session，是有用的第二道检测 |
| `LogOutput` | `os.Stderr` | **必须显式设置** | `VerifyConfig` 要求 `LogOutput`/`Logger` 二选一非空（`mux.go:86-91`）；默认值会往 CLI stderr 吐日志，污染输出 |
| 窗口与流数的预算 | — | `MaxStreams = min(--max-streams, StreamBudget/(2*Window))` | 128 MiB 预算下：256 KiB → 256 流；1 MiB → 64 流。A9 的两套预算由此自动对齐，启动时打印实际生效值 |
| **EOF 后的拆除期限** | — | **5 s**（`--pipe-drain`） | 双向 `io.Copy` 里一侧读到 EOF 后，给另一侧 5 s 把剩余数据送完；超时直接 `Reset()`。没有这条，对端不主动关连接时 goroutine 与 2×窗口 会永久残留 `[实测：500 连接随机掐断后需显式拆除才能让 goroutine 回落]` |
| **preamble 读超时** | — | **10 s**（`--handshake-timeout`，与 status 交换共用一个预算） | 流建立后必须在 10 s 内读完前置头，否则关流并回 `StatusBadRequest`。这是半开连接的**第二道**防线：第一道是 `--max-pending`（已 accept 但还没拿到流的本地连接上限 128，超出直接关 TCP），第二道就是这里——对端拿到了流却连上不说话（端口扫描器、健康检查、`nc` 后忘了打字）时，流会一直挂着占 2×窗口。C3 两层都验 |

### 2.5 转发器接口（`internal/forward`）

```go
package forward

// Mux is the slice of a multiplexed session the forwarder needs. Keeping it an
// interface lets the whole package be tested with a fake, with no yamux and no
// network.
type Mux interface {
    Open(ctx context.Context, kind byte) (Stream, error)
    NumStreams() int
}

type Stream interface {
    io.ReadWriteCloser
    SetDeadline(t time.Time) error
}

// Provider supplies the session that is currently live. The node reconnects
// and rebuilds the session on every drop (node.go:136-246), so a forwarder
// must never cache one: it asks for the current session per connection.
type Provider interface {
    Current(ctx context.Context) (Mux, error)
}

type Config struct {
    // Accept serves -L streams arriving from the peer. Off by default.
    Accept bool
    // AcceptReverse hosts listeners the peer asked for (-R). Off by default:
    // binding a port is a categorically bigger permission than dialling one.
    AcceptReverse bool
    // ListenAll permits a -R request to bind a non-loopback address.
    ListenAll bool
    Allow         AllowList   // zero value denies everything
    ListenPorts   PortSet     // zero value denies every -R port
    Limits        Limits
    // Dialer is swapped in tests to prove policy runs before dialling.
    Dialer        Dialer
    Logger        *slog.Logger
}

type Manager struct{ /* ... */ }

func New(cfg Config) *Manager

// AddLocal installs one -L listener. The listener is created ONCE and survives
// reconnects: rebuilding it per session would briefly unbind the port and race
// a concurrent bind (research §3.6).
func (m *Manager) AddLocal(sp Spec) (*LocalForward, error)

// RegisterReverse handles a peer's FrameForwardListen. It applies policy and
// returns one status per spec; it never returns an error, because a refusal is
// a normal answer, not a failure.
func (m *Manager) RegisterReverse(ctx context.Context, specs []wire.ForwardListenSpec) []wire.ForwardListenStatus

// Handle serves one inbound kind=1 stream: read preamble, check policy, dial,
// reply status, pipe.
func (m *Manager) Handle(ctx context.Context, st Stream)

// Serve is the kind dispatcher handed to mux.Session.Serve.
func (m *Manager) Serve(ctx context.Context, st Stream)

func (m *Manager) Close() error

// LocalForward is one -L mapping.
type LocalForward struct{ /* ... */ }
func (lf *LocalForward) Addr() net.Addr   // resolved address, for ":0" tests
func (lf *LocalForward) Serve(ctx context.Context, p Provider)
func (lf *LocalForward) Close() error

// Spec is one -L or -R entry, after parsing.
type Spec struct {
    Reverse bool
    Bind    string // "127.0.0.1:8080" — for -R this is a REQUEST to the peer
    Target  string // "db:5432"
    ID      string // Bind, normalized; -R matches the peer's stream by this
}
```

`Manager.Serve` 的分发逻辑（**必须**，这是 kind 语义的唯一执行点）：

```go
func (m *Manager) Serve(ctx context.Context, st Stream) {
    switch st.Kind() {
    case mux.KindForward:
        if !m.cfg.Accept {
            // Do not even read the preamble: answering Denied immediately
            // means a peer without permission cannot use us as a timing or
            // reachability oracle for its own targets.
            _ = wire.WriteStatus(st, wire.StatusDenied, "forwarding is disabled (--accept)")
            st.Close()
            return
        }
        m.Handle(ctx, st)
    case mux.KindControl:
        // Only the responder expects this, and only during bootstrap.
        m.sink(st)
    default:
        _ = wire.WriteStatus(st, wire.StatusBadRequest, "unknown stream kind")
        st.Close()
    }
}
```

---

## 3. 协议扩展

### 3.1 新增帧类型

```go
const (
    // ... 既有 FrameHello..FrameP2PAnswer 不变 ...

    // FrameStreamData carries an opaque byte block of the multiplexed byte
    // stream. Payload is raw bytes, <= MaxStreamData. The Frame.Stream field
    // MUST stay 0: multiplexing is yamux's job, and giving wire.Frame a second
    // stream id would let the two disagree.
    FrameStreamData

    // FrameMuxStart is the mux barrier. Payload is one byte: the mux protocol
    // version (1). Both peers send one and wait for the other's. No nonce is
    // needed: secure's strictly increasing counters already make a replayed
    // or reordered barrier impossible.
    FrameMuxStart

    // FrameForwardListen asks the peer to host listeners for -R. Payload is
    // JSON {"specs":[{"id","host","port"}]}.
    FrameForwardListen

    // FrameForwardListenResult answers it. Payload is JSON
    // {"results":[{"id","status","message"}]}.
    FrameForwardListenResult
)

// MaxStreamData bounds one FrameStreamData payload.
//
// 32 KiB, not peer.ChunkSize's 512 KiB. The 512 KiB figure came from loopback
// throughput (D11: 539.3 vs 211.9 MiB/s), and optimising for it makes every
// other stream on the connection wait behind a whole record: 250 ms on a
// 2 MiB/s link versus 15.6 ms here. AEAD still runs at 1408 MB/s at this size
// [measured], so nothing is lost by shrinking it — the write side recovers the
// syscall count by batching 16 records into one transport write.
const MaxStreamData = 32 << 10
```

`FrameForwardListen*` 走**控制流**（也就是 `peer.Session` 的帧分发），不新开 stream kind。理由：它们是一次性控制消息，天然属于会话控制面；`peer.Session` 已经有按类型分发的 `readPump`（`session.go:317-344`），加两个 case 即可。

### 3.2 转发握手 preamble

每条 `kind=1` 的流以一个小前置头开始，之后才是双向字节流。

```
偏移  长度  字段
0     1    version = 1
1     1    kind: 1 = direct (-L), 2 = reverse (-R)
2     1    addrLen (0..255)
3     n    addr (UTF-8)
```

- **`direct`（`-L`）**：`addr` 是**请求对端拨号的目标**（`"db:5432"`）。对端必须用白名单校验它。
- **`reverse`（`-R`）**：`addr` 是**请求方配置的监听器 ID**（`"127.0.0.1:9000"`），**不是拨号目标**。对端按这个 ID 查自己的 `-R` 配置，拨本地配置里的 target。

```go
package wire

const PreambleVersion byte = 1

const (
    KindDirect  byte = 1 // opener asks the acceptor to dial Addr
    KindReverse byte = 2 // opener reports which of the acceptor's -R listeners fired
)

type Preamble struct {
    Version byte
    Kind    byte
    Addr    string
}

// WritePreamble writes the 3+n byte header in a single Write.
func WritePreamble(w io.Writer, p Preamble) error

// ReadPreamble parses it strictly. Every field is attacker-controlled.
func ReadPreamble(r io.Reader) (Preamble, error)
```

**为什么 `-R` 要带一个 ID，而不是研究二说的「Addr 仅用于日志」：** 研究二的方案在只有一个 `-R` 时能工作，但有两条 `-R`（`9000→:3000`、`9001→:3001`）时，接收方无法知道对面那条流是从哪个监听器来的，只能猜。用一个确定性的 ID（= 请求的 `bind` 规范化后的 `host:port`）匹配，既不需要额外协商，也让「目标只来自本地配置」这条结构性保证成立：**ID 只用于查表，绝不用于拨号**。若 ID 不在本端配置里 → `StatusDenied`。

**严格解析规则【必须】**：

| 检查 | 拒绝条件 | 理由 |
|---|---|---|
| 版本 | `Version != 1` | 前向兼容 |
| kind | 不是 1/2 | 未知语义 |
| addrLen | `> 255` | 有界分配 |
| addr | 空、含 NUL 或控制字符、非 UTF-8 | 日志注入 / 解析歧义 |
| addr | `net.SplitHostPort` 失败 | 必须是 `host:port` |
| host | 空 | `:22` 会被解析成任意本地地址 |
| port | 非数字、`<1`、`>65535` | `port 0` 会让 dial 语义变成「随机端口」 |

### 3.3 状态码

```go
type Status uint32

const (
    StatusOK         Status = 0 // dialled; the stream is now a byte pipe
    StatusDenied     Status = 1 // policy: not allowed (allow-list, no --accept, unknown listener)
    StatusDialFail   Status = 2 // policy allowed it, the target did not answer
    StatusTooMany    Status = 3 // stream or rate limit
    StatusBadRequest Status = 4 // malformed preamble, unknown kind/version
    StatusBindFail   Status = 5 // only in ForwardListenResult: listener could not bind
)

// Wire form: status(4 BE) then an optional reason:
//   reasonLen(2 BE) + reason(<=200 bytes UTF-8)
// The reason is for the operator, never for control flow.
func WriteStatus(w io.Writer, s Status, reason string) error
func ReadStatus(r io.Reader) (Status, string, error)
```

研究二 §3.2 的实测教训是「没有状态码时，被拒绝的连接只表现为流立刻关闭，客户端分不清策略拒绝和目标挂了」。加了状态码后，日志从「连接被重置」变成明确的 `peer refused target ... (status 1)` `[实测]`。

本设计在研究二 4 个状态码的基础上加了 `BadRequest` 和 `BindFail`：
- `BadRequest` 让协议错误与策略拒绝可区分（否则畸形 preamble 会被记成「被拒绝」，掩盖攻击或 bug）；
- `BindFail` 让 `-R` 注册失败（端口被占用、权限不足）能与「策略拒绝」区分，用户才知道该换端口还是改配置。

**顺序是安全属性，不是风格**【必须】：

```go
// 先查策略，再拨号。顺序反了，被拒绝的请求也会产生一次出站连接，
// 攻击者就能用它做端口扫描（响应时间 + 状态码）和放大。
if ip, err := m.allow.Permits(ctx, p.Addr); err != nil {
    _ = wire.WriteStatus(st, wire.StatusDenied, err.Error())  // 不拨号
    return
}
target, err := m.dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
if err != nil {
    _ = wire.WriteStatus(st, wire.StatusDialFail, "target unreachable")
    return
}
_ = wire.WriteStatus(st, wire.StatusOK, "")
pipe(st, target)
```

### 3.4 `wire.Version`：不升到 2（对 ARCHITECTURE-SDWAN §A1 的修正）

ARCHITECTURE-SDWAN §A1 的结论是「新增帧类型与 mux 属于不兼容变更，`wire.Version` 提升到 2」。**本设计不同意，理由是代码事实：**

| 事实 | 证据 |
|---|---|
| 帧格式完全不变：10 字节头（type/flags/stream/length），payload ≤ 1 MiB | `frame.go:104-130` `[查证]` |
| 新增帧类型是纯增量：`readPump` 的 `default` 分支忽略未知类型 | `session.go:341-343` `[查证]` |
| 版本不一致时**没有降级路径**，直接 `ErrProtocol` | `session.go:244-246` `[查证]` |

也就是说：升到 2 之后，**新二进制的 `send` 无法与旧二进制的 `recv` 通信，连传文件都不行**。这是为了一个「不存在的不兼容」而制造一个真实的功能倒退。项目自己的原则（拒绝 smux，因为它静默丢数据）就是「宁可明确失败，也不要隐蔽地坏」，但这里根本不需要失败。

**决定【必须】：`wire.Version` 保持 1；用 hello 的 `caps` 协商 mux。**

```go
// hello payload（既有字段不变，新增两个可选字段）
type Hello struct {
    Version int      `json:"version"`          // 仍是 1
    Name    string   `json:"name"`
    Role    string   `json:"role"`
    // Caps advertises optional capabilities. Absent means "none", which is
    // exactly what a v1 binary sends: it never enters the mux path, and its
    // file transfer is untouched.
    Caps    []string `json:"caps,omitempty"`   // "mux", "forward"
    Mux     int      `json:"mux,omitempty"`    // mux protocol version (1)
}
```

判定逻辑：

| 本端 | 对端 | 结果 |
|---|---|---|
| `send`/`recv`（不设 `UseMux`） | 任意 | **不建 mux**，代码路径与今天逐字节相同 |
| `forward` | 旧版（无 `caps`） | `ErrProtocol` + 明确提示「peer does not support forwarding; upgrade both sides」 |
| `forward` | `forward` | 建 mux，正常转发 |
| `forward` | `send` | 同上：对端没有 `caps` → 明确拒绝 |

**什么时候才该升版本**：改动帧头布局、改变既有帧类型的语义、或需要单方面拒绝旧对端时。届时 `caps` 机制让升级成为一行改动（把 `Version` 改成 2 即可），所以本设计对两种选择都兼容——只是当前没有理由选 2。

**这条修正必须同步写进 `ARCHITECTURE-SDWAN.md` §A1**，否则实现者会照旧文档升版本。

### 3.5 握手时序

**屏障（两端对称）**

```
A（initiator / yamux client）                B（responder / yamux server）
  │ hello(version=1,caps=[mux,forward],mux=1) │
  │<──────────────────────────────────────────>│
  │ (可选) p2p 协商 + UpgradeToDirect           │   ← 必须在 mux 之前（A8）
  │ FrameMuxStart{1} ─────────────────────────>│
  │<───────────────────────── FrameMuxStart{1} │
  │ ── 两边同时切换：读/写 goroutine 启动 ──    │
  │ OpenStream(kind=0) ───────────────────────>│
  │ 控制流上跑 peer.Session（消息/文件）        │
```

**`-L`（本地监听 → 对端拨号）**

```
本地 cmesh                                   远端 cmesh
  │ accept() on 127.0.0.1:8080                 │
  │ OpenStream(kind=1) ───────────────────────>│
  │ Preamble{v1, direct, "db:5432"} ──────────>│
  │                                            │ allow.Permits → 得到 IP
  │                                            │ 不在白名单 → Status(1)，不拨号
  │                                            │ DialTimeout(5s) 拨 IP
  │<──────────────────────── Status(0) ────────│
  │<────────────── 双向字节 + 半关闭 ──────────>│
```

**`-R`（对端监听 → 回连本地）**

```
本地 cmesh（-R 请求方）                       远端 cmesh
  │ FrameForwardListen{[{id:"127.0.0.1:9000",   │
  │   host:"127.0.0.1", port:9000}]} ─────────>│
  │                                            │ --accept-reverse? 
  │                                            │ --allow-listen-port 含 9000?
  │                                            │ loopback? 否则要 --listen-all
  │                                            │ bind → 失败则 StatusBindFail
  │<───────────── ForwardListenResult ─────────│
  │                                            │ (远端应用 connect 127.0.0.1:9000)
  │                                            │ OpenStream(kind=1) ─────────>
  │<── Preamble{v1, reverse, "127.0.0.1:9000"} │
  │ 按 ID 查本地 -R 配置 → target              │
  │ 查不到 → Status(1)                         │
  │ 拨 target（本地配置，不是 preamble）       │
  │<──────────────── Status(0) ───────────────>│
  │<──────────── 双向字节 + 半关闭 ───────────>│
```

---

## 4. CLI 设计

### 4.1 语法

```
cmesh forward [连接参数] [转发规格] [策略参数]
```

```bash
# ssh -L 等价：本地 8080 → 对端拨 db:5432
cmesh forward --code CODE -L 127.0.0.1:8080:db:5432

# 多条转发，一条隧道
cmesh forward --code CODE \
    -L 127.0.0.1:8080:db:5432 \
    -L 127.0.0.1:9090:api:80

# ssh -R 等价：请对端监听 127.0.0.1:9000，回连我这里的 127.0.0.1:3000
cmesh forward --code CODE -R 127.0.0.1:9000:127.0.0.1:3000

# 纯接受端：不开本地监听，只服务对端的 -L
cmesh forward --code CODE --accept \
    --allow-host db.internal --allow-port 5432 --expect-sas 123456

# 同时接收文件（同一个 session）
cmesh forward --code CODE -L 127.0.0.1:8080:db:5432 --also-recv-files --out ./inbox
```

### 4.2 参数表

**连接参数（复用 `commonFlags`，`cli.go:117-141`）**

| 参数 | 默认 | 说明 |
|---|---|---|
| `--relay` | `ws://127.0.0.1:8080/ws` | 逗号分隔多 relay，按序故障切换（env `CMESH_RELAY`） |
| `--code` | 空（自动生成并打印） | 共享口令（env `CMESH_CODE`） |
| `--name` | 主机名 | 对端可见的名字 |
| `--pair-window` | 0（单 relay 永久等待） | 一个 relay 上等对端的时长 |
| `--p2p` / `--p2p-legacy` / `--stun` | false / false / 内置 | 直连打洞，与 `send`/`recv` 一致 |
| `--expect-sas` | 空 | **启用转发时强烈建议**；不一致直接中止（D38） |

**转发规格（可重复）**

| 参数 | 语法 | 说明 |
|---|---|---|
| `-L [bind:]port:host:port` | 最少 3 段 | 本地监听 `bind:port`（bind 省略 = `127.0.0.1`），对端拨 `host:port` |
| `-R [bind:]port:host:port` | 最少 3 段 | **请求**对端监听 `bind:port`（省略 = `127.0.0.1`），本端拨 `host:port` |

**接受端策略（本端服务对端）**

| 参数 | 默认 | 层 | 说明 |
|---|---|---|---|
| `--accept` | **false** | 1 | 允许服务对端的 `-L` 流 |
| `--accept-reverse` | **false** | 1b | 允许托管对端请求的 `-R` 监听器 |
| `--allow-host HOST\|CIDR` | 空 = **拒绝一切** | 2 | 可重复；主机名在启动时解析并**固定成 IP** |
| `--allow-port PORT\|LO-HI` | 空 = 任意端口 | 2 | 可重复；仅在 `--allow-host` 非空时有意义 |
| `--allow-listen-port PORT\|LO-HI` | 空 = **拒绝一切** | 2b | 可重复；`-R` 请求的端口必须命中 |
| `--listen-all` | false | 3 | 允许 `-R` 绑非 loopback；启动时打印警告 |
| `--deny-close N` | 0（从不） | 3b | 连续 N 次拒绝后关闭会话 |

**上限与调优**

| 参数 | 默认 | 说明 |
|---|---|---|
| `--max-streams N` | 256 | 并发流转发上限；会被内存预算压低（见下） |
| `--max-streams-per-sec N` | 32 | 防连接风暴 |
| `--max-pending N` | 128 | 已 accept 但还没拿到 mux 流的本地连接数；超出直接关 TCP |
| `--dial-timeout D` | 5s | 防半开连接堆积 |
| `--handshake-timeout D` | 10s | preamble + status 交换的上限 |
| `--max-lifetime D` | 0（不限） | 到点强制关流 |
| `--mux-window SIZE` | 256KiB | yamux `MaxStreamWindowSize`；WAN 建议 1MiB |
| `--stream-budget SIZE` | 128MiB | `MaxStreams × 2 × Window` 的上限；启动时打印实际生效的流数 |

**其他**

| 参数 | 默认 | 说明 |
|---|---|---|
| `--also-recv-files` | false | 在同一 session 上接受文件（复用现有 `OnOffer`） |
| `--out DIR` | `.` | `--also-recv-files` 的落盘目录 |
| `--quiet` | false | 只打错误 |
| `--json` | false | 结构化日志（便于 CI 断言） |

### 4.3 `-L`/`-R` 解析

**从右往左切两段 `host:port`，剩下的作为 bind（或本地端口）。** 必须处理方括号。

```go
// parseSpec splits "[bind:]port:host:port" without breaking on IPv6 colons.
//
// The grammar is deliberately right-associative: the last two fields are the
// target, and whatever remains is the local side. That makes
//   8080:db:5432           -> local port 8080,  target db:5432
//   127.0.0.1:8080:db:5432 -> local 127.0.0.1:8080, target db:5432
//   [::1]:8080:db:5432     -> local [::1]:8080, target db:5432
//   8080:[::1]:5432        -> local 8080,      target [::1]:5432
// all unambiguous. Fewer than three fields is rejected rather than guessed.
func parseSpec(s string, reverse bool) (Spec, error)
```

表驱动测试必须覆盖（ARCHITECTURE-SDWAN §6 明确要求）：`8080:h:1`、`1.2.3.4:8080:h:1`、`[::1]:8080:h:1`、`8080:[::1]:1`，外加：
`8080:db:5432`（省略 bind）、`[::1]:8080:[::2]:5432`（两端 IPv6）、`8080:db:5432:extra`（4 段但 bind 非法）、`8080:db`（2 段，拒绝）、`8080:db:0`（port 0 拒绝）、`0.0.0.0:8080:db:5432`（`-L` 允许但警告；`-R` 需要 `--listen-all`）、`[::1]:8080:db:5432` 与 `::1:8080:db:5432`（后者必须拒绝并提示加方括号）。

`-L 0:db:5432`（ephemeral 端口）**允许**并打印实际端口——测试与临时调试很有用。

### 4.4 与 `send`/`recv` 的共存

| 问题 | 决定 | 理由 |
|---|---|---|
| `forward` 能不能同时收文件？ | **能**，`--also-recv-files` | 控制流上跑的就是完整的 `peer.Session`，`FrameFileOffer`/`Chunk`/`Done` 全部照旧，零额外协议。研究二 §3.5 也建议了这一点 |
| `forward` 能不能发文件？ | **v1 不做** | 发文件需要 REPL（`/file PATH`）或一次性参数；`forward` 的终端应留给状态输出。**可选**后续加 `--send-file` |
| `send`/`recv` 会不会建 mux？ | **不会** | `UseMux=false` 时完全不进入 mux 路径，行为与今天逐字节相同。这是既有 e2e 套件（39 个断言）不退化最重要的保证 |
| `forward` 对上 `send`/`recv`？ | 明确失败 | hello 的 `caps` 没有 `mux` → `ErrProtocol` + 「peer does not support forwarding; upgrade both sides」。不会静默降级成「能连但转发不工作」 |
| `forward` 对上 `forward`，但两边都没有 `-L`/`-R`？ | 允许 | 纯接受端（只 `--accept`）是合法用法 |
| 两边都没有规格也没有 `--accept*`？ | 启动即报错 | 「nothing to do」比连上后无事可做更好 |

### 4.5 启动横幅（**必须**）

启动时**必须**打印一行摘要，让操作者看清自己暴露了什么。这是第四层防御的「人」的部分：

```
[cmesh] forward: 2 local listeners, accepting=-L:yes(-R:no)
[cmesh]   -L 127.0.0.1:8080 -> db:5432
[cmesh]   -L 127.0.0.1:9090 -> api:80
[cmesh]   allow: db.internal(10.0.0.7/32), 10.0.0.0/8 ports 5432,6379
[cmesh]   mux window 256KiB, max streams 256 (budget 128MiB), dial timeout 5s
[cmesh]   SAS 123456 (compare out of band; pass --expect-sas to enforce)
[cmesh] WARNING: --accept is on without --expect-sas: a leaked code is enough
        to reach every allow-listed target
```

`--accept` 且没有 `--expect-sas` 时**必须**打警告；`--listen-all` 时必须打更强的警告。

---

## 5. 安全设计

### 5.1 威胁模型

cmesh 的信任模型是「知道 code 的人就是合法对端」（`secure.go:4-15`）。但 **code 会泄露**：截图、终端滚动历史、共享屏幕、CI 日志、`CMESH_CODE` 环境变量泄漏。而转发的破坏力**远大于**传文件：

- 传文件：攻击者能写一个文件到工作目录（已有 `--allow-hidden`、覆盖保护、16 GiB 上限）。
- `-L` 转发：攻击者能**连接容器能到达的任何地址**，包括云元数据服务、内网数据库、集群 API。
- `-R` 转发：攻击者能**在容器里开一个监听端口**，向本地进程提供内容。

结论：转发必须默认关闭，且关闭的默认值必须是**结构性的**（零值即拒绝），不能靠「记得配」。

### 5.2 四层防御（全部默认拒绝）

#### 第 1 层：默认不启用

| 开关 | 默认 | 保护什么 |
|---|---|---|
| `--accept` | false | 不服务任何入站 `-L` 流 |
| `--accept-reverse` | false | 不托管任何 `-R` 监听器 |

分成两个开关而不是一个，是因为两种权限的量级不同：`--accept` 是「允许你拨我允许的地址」，`--accept-reverse` 是「允许你在我这里开端口」。研究二 §3.4 只用一个开关（且正文的 `--allow-forward` 与参数表的 `--accept` 不一致），**不够**。

实现上，`--accept=false` 时收到 `kind=1` 流**连 preamble 都不读**就回 `StatusDenied` 并关闭——这样即使 preamble 解析器有漏洞，未授权的对端也触达不到它。

#### 第 2 层：目标白名单（结构性）

```go
// AllowList decides whether a target may be dialled.
//
// The zero value permits NOTHING. That is deliberate: a forgotten
// configuration must not turn the container into an open proxy. Note that an
// empty Hosts list is not "allow all" — it is "allow none".
type AllowList struct {
    // hosts are pinned at startup: a hostname entry is resolved once, and the
    // resulting IPs are what get compared. Re-resolving per dial would let a
    // DNS answer change between the check and the connect.
    hosts []hostRule   // {name string; ips []net.IP; cidr *net.IPNet}
    ports PortSet      // empty = any port
    resolver Resolver  // injectable for tests
}

// Permits returns the concrete IP to dial, or an error explaining the refusal.
//
// Returning the IP (not a bool) is what closes the check-then-dial window: the
// caller dials exactly the address that was authorised. A bool would force the
// caller to re-resolve the name, which is a TOCTOU bug.
func (a AllowList) Permits(ctx context.Context, addr string) (net.IP, error)
```

**必须**的解析期拒绝（不是警告，是错误）：

| 输入 | 结果 | 理由 |
|---|---|---|
| 零值 `AllowList` | 拒绝一切 | 忘记配置不能等于开放 |
| `--allow-host 0.0.0.0/0` | **启动失败** | ARCHITECTURE-SDWAN §8 明确「不做 `--allow-any`」。要放宽就写具体 CIDR |
| `--allow-host ::/0` | **启动失败** | 同上 |
| `--allow-host` 解析失败 | 启动失败 | 静默忽略一个白名单项比报错危险得多 |
| `--allow-port` 空但 `--allow-host` 非空 | 允许任意端口 | 与 ssh 的 `AllowTcpForwarding local` 一致；文档写明「建议同时限制端口」 |

**`-R` 的监听端口白名单（第 2b 层）**：

```go
// PortSet gates which ports a peer may ask us to listen on. The zero value
// denies every port. This is a separate list from AllowList.Ports because the
// risk is different: AllowList.Ports bounds what we dial out to, PortSet
// bounds what we expose inward.
type PortSet struct{ ranges []portRange }  // zero value = deny all
```

为什么必须有：没有它，持有 code 的攻击者可以让受害容器 bind `127.0.0.1:8080`，然后向本地进程提供内容（冒充本地开发服务器、抢在真正服务启动前占位、给本地 CLI 喂恶意响应）。研究二 §3.4 的 `--accept` 无法阻止这一点。

#### 第 3 层：监听地址约束

- `-L` 的 bind 默认 `127.0.0.1`（不是 `0.0.0.0`）。`-L 0.0.0.0:8080:...` 允许但打印警告。
- `-R` 的 bind 是**请求**，由**对端**裁决：非 loopback 必须对端有 `--listen-all`，否则 `StatusDenied`。
- **这条与 ssh 的 `GatewayPorts no` 是同一个设计**，而且裁决权必须在暴露端口的那一侧——如果让请求方自己决定，防线就不存在。

#### 第 4 层：流数 / 速率 / 超时

| 限制 | 默认 | 触发时 |
|---|---|---|
| `MaxStreams` | 256（受预算压低） | `StatusTooMany`，流被关闭，进程不受影响 |
| `MaxStreamsPerSec` | 32 | `StatusTooMany` |
| `MaxPending` | 128 | 直接关闭已 accept 的本地 TCP 连接（不消耗 mux 流） |
| `DialTimeout` | 5s | `StatusDialFail` |
| `HandshakeTimeout` | 10s | 关闭该流（opener 侧读 status 也有同样期限） |
| `MaxLifetime` | 0 | 到点关闭 |
| `deny-close` | 0 | 连续拒绝 N 次后关闭整个会话 |

内存上界的算式（**必须**在启动时算并打印）：

```
每活跃流 ≈ 2 × Window          （研究二 §4.5 [实测]：256KiB→514.8KiB，1MiB→2.0MiB）
MaxStreams = min(--max-streams, StreamBudget / (2 × Window))
```

128 MiB 预算下：256 KiB 窗口 → 256 流；1 MiB 窗口 → 64 流。ARCHITECTURE-SDWAN A9 的「两套预算各自硬顶」由此落地为一条可执行公式。

#### 额外要求（不是「层」，但必须做）

1. **`--expect-sas` 强烈建议**：`--accept` 且未设 `--expect-sas` 时启动警告。理由：转发是持续双向通道，MITM 长期利用的后果比一次性传文件严重（D38 的方向）。
2. **`-R` 的拨号目标只来自本地配置**，preamble 的 `Addr` 只用于查表，绝不直接拨。这是结构性保证：即使对端发来 `Addr="169.254.169.254:80"`，本端也只会查自己的 `-R` 表，查不到就拒绝。
3. **审计日志**：每次允许/拒绝都记 `peer`, `target`, `status`, `stream_id`, `duration`。日志只在本地，relay 只能看到密文。
4. **不做 `--allow-any`**。ARCHITECTURE-SDWAN §8 已明确列为「不做的事」。

### 5.3 攻击场景推演（code 已泄露）

假设攻击者拿到了完整 code，能完成 SPAKE2，因此 SAS 也一致（SAS 只防主动 MITM，不防 code 泄露）。逐场景分析：

| # | 攻击 | 默认配置下的结果 | 若操作者放宽了配置 | 限制手段 |
|---|---|---|---|---|
| 1 | 当 `-L` 的 opener，请求拨 `169.254.169.254:80`（云元数据） | `--accept=false` → 连 preamble 都不读，`StatusDenied` | 若白名单写了 `169.254.169.254` 才会成功 | 白名单零值拒绝；文档明示「不要把 link-local 写进白名单」 |
| 2 | 端口扫描内网：快速尝试 1000 个 `host:port` | 全部 `StatusDenied`；速率限制 32/s；连续拒绝可触发 `deny-close` | 若白名单是 `10.0.0.0/8` + 任意端口，则**扫描成功**——这是配置问题，不是实现问题 | 白名单要求具体端口；启动横幅明确打印生效范围 |
| 3 | 用状态码区分「在白名单但挂了」和「不在白名单」 | 能区分（`DialFail` vs `Denied`）。这是刻意的：运维需要它 | 同上 | 速率限制 + 审计日志 + `deny-close`；文档写明「状态码是诊断信息，也是探测信息」 |
| 4 | 请求 `-R` 监听 `127.0.0.1:6379`（抢占本地 Redis 端口） | `--accept-reverse=false` → `StatusDenied` | 若开了 `--accept-reverse` 但 `--allow-listen-port` 不含 6379 → 仍拒绝 | 端口白名单零值拒绝 |
| 5 | 请求 `-R` 监听 `0.0.0.0:9000`（对局域网暴露） | 对端 `--listen-all=false` → `StatusDenied` | 需要操作者显式开 `--listen-all` 并看到警告 | 裁决权在暴露侧 |
| 6 | 开 10000 条流耗尽内存 | `MaxStreams=256` → 第 257 条起 `StatusTooMany`；内存上限 128 MiB | — | 信号量 + 预算公式 |
| 7 | 打开 `kind=0` 想劫持控制流 | responder 只接受**一次**控制流，第二次 → 协议错误 → 关会话 | — | 控制流只接受一次；initiator 侧收到 `kind=0` 直接判协议错误 |
| 8 | 发畸形 preamble（255 字节、无端口、NUL、非 UTF-8） | 严格解析器拒绝 → `StatusBadRequest`；fuzz 测试覆盖 | — | §3.2 的检查表 |
| 9 | DNS rebinding：白名单写 `db.internal`，攻击者控制其解析 | 启动时解析并固定 IP；`Permits` 返回该 IP，拨号只用它 → **攻击失败** | 若显式开 `--allow-resolve-per-dial`（本设计不提供）才会成功 | check 与 dial 用同一个 IP |
| 10 | 通过转发做数据外泄（读允许目标的响应） | 只能读白名单内的服务。**这是授权的本意** | — | 白名单粒度；`--max-lifetime` 限制持续时间 |
| 11 | 半开连接堆积：开流后不发 preamble | `HandshakeTimeout=10s` 关闭 | — | 读 preamble 带 deadline |
| 12 | 大量本地连接打 `-L` 监听器（耗尽 fd/goroutine） | `MaxPending=128` 后直接关 TCP；`MaxStreams` 限制 mux 流 | — | 两层队列上限 |
| 13 | 慢速读（slowloris）：开流后不读，撑满窗口 | 该流自己阻塞在窗口上，其他流不受影响（每流独立流控 `[实测]`：一条流在 192 KiB 处精确阻塞，健康流仍 1251–1680 MiB/s） | — | yamux 每流窗口；`StreamCloseTimeout` 回收废弃流 |
| 14 | 冒充对端让本端连到错误的 relay | 与转发无关，属既有威胁模型；`--expect-sas` 防主动 MITM | — | D38 |

**必须诚实说明的边界**：如果 code 真的泄露且白名单配置宽松，**转发就是授权给攻击者的能力**。没有任何协议层机制能在「持有同一 code」的前提下区分合法用户与攻击者——那需要额外的身份（证书、OIDC）。本阶段不做，但要在文档里写清楚，并建议：`--accept` 只用于可信网络；敏感服务用 `--allow-host` + 具体端口；启用转发时设 `--expect-sas`。

### 5.4 防御清单（实现者逐条核对）

- [ ] `--accept` 默认 false；为 false 时不读 preamble 就拒绝
- [ ] `--accept-reverse` 默认 false
- [ ] `AllowList` 零值拒绝一切；空 `Hosts` ≠ 允许全部
- [ ] `--allow-host 0.0.0.0/0`、`::/0` **启动失败**（不是警告）
- [ ] 白名单主机名启动时解析并固定 IP；`Permits` 返回 IP
- [ ] 拨号只用 `Permits` 返回的 IP，不再解析名字
- [ ] `PortSet`（`--allow-listen-port`）零值拒绝一切 `-R`
- [ ] `-R` 的非 loopback bind 需要**对端** `--listen-all`
- [ ] `-L` 的 bind 默认 `127.0.0.1`
- [ ] 策略检查在 dial 之前（有单测证明 dialer 未被调用）
- [ ] `MaxStreams` / `MaxStreamsPerSec` / `MaxPending` / `DialTimeout` / `HandshakeTimeout`
- [ ] 流数被内存预算压低，启动时打印实际值
- [ ] 控制流只接受一次
- [ ] preamble 严格解析（版本/kind/长度/UTF-8/host/port）
- [ ] 所有拒绝都有审计日志，日志不含 code
- [ ] `--accept` 无 `--expect-sas` 时启动警告
- [ ] 不存在 `--allow-any`
- [ ] 启动横幅打印完整暴露面

---

## 6. 测试计划

### 6.1 单元测试

| 包 | 用例 | 断言 |
|---|---|---|
| `wire` | preamble 往返 | 4 种 IPv4/IPv6 组合字节精确 |
| `wire` | preamble 拒绝表 | 版本≠1、kind∉{1,2}、len>255、空、NUL、非 UTF-8、无端口、port 0/65536、空 host |
| `wire` | `FuzzReadPreamble` | 任意输入不 panic、不越界分配（10 分钟无 crash） |
| `wire` | status 往返 | 5 个状态码 + 可选 reason；reason 超 200 字节被截断 |
| `wire` | `ForwardListen` JSON 往返 | 空列表、重复 ID、超长 ID |
| `peer/pump` | 引导直通 | 封/开出的帧与改造前逐字节一致；计数器序列一致 |
| `peer/pump` | 屏障 | 两端各发 `FrameMuxStart` 后切换；屏障前到达的非 mux 帧被丢弃且不致命 |
| `peer/pump` | **顺序不变量** | 8 个 goroutine 并发 `Write` 32 MiB → 对端 `OpenFrame` 计数器严格递增、无解密失败（这是整个设计的核心性质） |
| `peer/pump` | 背压 | 读侧停 2 s，写侧阻塞而不丢字节；恢复后字节数与 SHA-256 一致（对照研究二 §4.4 的 yamux 结论） |
| `peer/pump` | 传输切换 | `Recv` 返回 `ErrTransportSwitched` 后泵继续，不关会话 |
| `peer/pump` | 关闭 | `Close` 后 `Read`/`Write` 都返回错误；无 goroutine 泄漏（`goleak`） |
| `peer/frameconn` | 短写 | 底层 `Write` 返回 `n<len` 时 `writeFull` 补齐 |
| `peer/frameconn` | 并发 `SendFrame` | 100 个并发小帧 → 对端逐帧解出，无交错 |
| `peer/session` | 委托访问器 | `ShortAuth`/`Fingerprint`/`FramesSinceStart` 在 mux 前后都正确 |
| `mux` | kind 路由 | `KindControl`/`KindForward`/未知 kind 各自的行为 |
| `mux` | 流 ID 奇偶 | client 开奇数、server 开偶数（防两端都选 client） |
| `mux` | `MaxStreams` | `Open` 在第 N+1 条阻塞；`Accept` 返回 `Limited()==true` |
| `mux` | 信号量归还 | 每条流 `Close` 恰好归还一次（重复 `Close` 不 double-free） |
| `mux` | 半关闭 | `CloseWrite` 后对端读到 EOF，且仍能写回（研究二 §4.2 的同形态断言） |
| `mux` | `LogOutput` | 配置后 yamux 日志不出现在 stderr（用 buffer 断言） |
| `forward/allow` | 零值 | `AllowList{}` 拒绝一切 |
| `forward/allow` | CIDR / 主机名固定 | 启动解析一次；模拟 DNS 改变后仍拨旧 IP |
| `forward/allow` | `0.0.0.0/0` | 构造期返回错误 |
| `forward/allow` | 端口/端口段 | `5432`、`6000-7000`、边界 |
| `forward` | 策略先于 dial | 注入假 `Dialer`，被拒绝时断言调用次数为 0 |
| `forward` | `-R` 目标只来自本地 | preamble 里放 `169.254.169.254:80`，断言只拨本地配置的 target |
| `forward` | 速率限制 | 32/s 后返回 `TooMany` |
| `forward` | 半关闭 pipe | `net.Pipe` + 假 stream，HTTP/1.0 形态不挂起 |
| `cli` | `parseSpec` 表驱动 | §4.3 全部用例 + 4 段非法 + 2 段拒绝 + 无方括号 IPv6 |
| `cli` | 策略组合 | `--accept-reverse` 缺 `--allow-listen-port` → 启动失败并给指引 |
| `cli` | 预算公式 | `--mux-window 1MiB` + `--max-streams 256` → 生效值 64，横幅打印 |

### 6.2 集成测试（真实 TCP）

| 用例 | 步骤 | 门槛 |
|---|---|---|
| I1 `-L` 并发正确性 | 对端起 echo 服务；`forward -L`；10 并发 × 8 轮 × 512 KiB，每流独立字节模式 | 10/10 字节精确（对照研究二 §4.1 原型 `[实测]`） |
| I2 半关闭 | HTTP/1.0 客户端发请求后 `CloseWrite`，等服务端响应 | 收到完整响应，不挂起 |
| I3 服务端先说话 | 假 SMTP banner（连接即发 `220 ...`） | 客户端在发任何字节前收到 banner |
| I4 大文件 | 1 GiB 经 `-L` 转发，双向 SHA-256 | 一致；吞吐见 P2 |
| I5 多条规格 | 2 个 `-L` + 1 个 `-R` 同一 session 并发 | 三条路径同时可用，互不干扰 |
| I6 `-R` 端到端 | 远端 listener accept，本端拨本地 target | 字节精确；对端应用能连上 |
| I7 白名单拒绝 | 目标不在白名单 | `StatusDenied`；对端目标**没有**收到连接（用只记录不响应的 listener 断言） |
| I8 默认拒绝 | 不传 `--accept` | 所有入站 `-L` 流被拒 |
| I9 `-R` 授权 | 不传 `--accept-reverse` / 传了但 `--allow-listen-port` 不含该端口 | `StatusDenied`，远端没有 bind |
| I10 `--listen-all` | 请求 `0.0.0.0:9000`，远端未开 `--listen-all` | `StatusDenied` |
| I11 流数上限 | 开到 257 条 | 第 257 条 `StatusTooMany`，进程存活，已有流不受影响 |
| I12 重连 | 杀 relay → 重启 → 旧连接 EOF → 新连接可用 | listener 仍在（不重新 bind），新连接成功 |
| I13 版本互操作 | `forward` ↔ `send` | 明确报错「peer does not support forwarding」，不静默 |
| I14 文件共存 | `--also-recv-files` + 转发同时进行 | 文件 SHA-256 一致，转发吞吐下降 < 20% |
| I15 控制流劫持 | 对端主动开 `kind=0`（模拟攻击） | 会话被关闭，日志有记录 |

### 6.3 安全测试

| 用例 | 方法 | 通过条件 |
|---|---|---|
| S1 preamble fuzz | `go test -fuzz=FuzzReadPreamble -fuzztime=10m` | 无 panic、无 OOM（配合 `-race`） |
| S2 code 泄露 + 默认配置 | 用正确 code 起一个攻击者客户端，尝试 100 个内网目标 | 100% 被拒，目标侧连接计数为 0 |
| S3 code 泄露 + 宽松白名单 | 白名单 `10.0.0.0/8` + 任意端口 | 记录为**已知风险**：这是配置授权，不是漏洞；断言有审计日志 |
| S4 拒绝探测 | 交替请求「在白名单但不可达」与「不在白名单」 | 状态码可区分（`DialFail` vs `Denied`），速率受限，日志完整 |
| S5 rebinding | 假 resolver：第一次返回白名单 IP，第二次返回攻击者 IP | 拨的是第一次的 IP |
| S6 `0.0.0.0/0` | 构造 `AllowList` | 返回错误，进程拒绝启动 |
| S7 `-R` 端口抢占 | 请求 bind `127.0.0.1:<一个空闲端口>`，`--allow-listen-port` 不含它 | 拒绝；该端口仍可被本地进程 bind |
| S8 流洪泛 | 1 秒内 1000 次 open | 429 级拒绝；RSS 增量 < 预算；无 goroutine 爆炸 |
| S9 控制流劫持 | 见 I15 | 会话关闭 |
| S10 日志脱敏 | 全流程抓 stderr | 不出现 code；不出现完整 target（只出现请求方给的字符串或本地配置，二者都不是秘密，但要确认没有把解析后的内网 IP 回给对端） |

### 6.4 性能测试

**基线**：现有文件传输回环 512 MiB = **539.3 MiB/s**（README/D11 `[实测]`）。

| 编号 | 指标 | 门槛 | 依据 |
|---|---|---|---|
| P1 | 文件传输回归（512 MiB，512 KiB 分块） | **≥ 485 MiB/s**（下降 ≤ 10%） | ARCHITECTURE-SDWAN A10 的回归门槛 |
| P2 | 单流 `-L` 转发吞吐（回环，512 MiB） | **≥ 200 MiB/s** | ARCHITECTURE-SDWAN 验收 1.7。`[推断]` 实际应在 300–500 MiB/s（库层 yamux+AEAD 1957–2748 MiB/s，全栈参照 539 MiB/s） |
| P3 | 10 流聚合（回环） | **≥ 500 MiB/s** | 验收 1.7。研究二实测 10 流 1962 MiB/s（库层），全栈打折后仍应远超 500 |
| P4 | 连接建立延迟（`-L`，2000 次） | p50 ≤ 2 ms，p95 ≤ 5 ms | 每次连接 = TCP accept + stream open + preamble + status（2 个 RTT 级） |
| P5 | 已建立流的往返延迟（2000 次 ping-pong） | p50 ≤ 1 ms，p95 ≤ 3 ms | 研究二 yamux 空闲 p50 18 µs、9 条满载 191 µs `[实测]`；留出全栈余量 |
| P6 | 100 条活跃流转发时 RSS | **< 80 MiB** | ARCHITECTURE-SDWAN 验收 1.9 |
| P7 | 100 条空闲流转发时 RSS | < 40 MiB | 每空闲流 1.7–2.5 KiB `[实测]` |
| P8 | relay 消息数 | 记录实测值，参照 **≤ 13 条/MiB**（研究二实测值） | 32 KiB 记录对应约 32 条/MiB。这个数字偏高是**已知且接受的代价**：换来的是 15.6 ms 而非 250 ms 的延迟下限。若 relay 的 `MaxMessageBytes` 或限流承压，再考虑传输层批量（§2.1） |
| P9 | 文件传输 + 转发并发 | 转发吞吐下降 < 20%，文件传输下降 < 15% | 单 `sendLoop` 串行化是已知代价 |

**P2–P5 的 `[推断]` 门槛必须在实现后第一次实测时替换为真实数字**，并把实测值写回本表——不能沿用推断值冒充验收。

---

### 6.5 混沌测试（评审补充，G1 门槛）

这四项不是「锦上添花」，而是针对本设计三个最可能翻车的地方（队头阻塞、内存无界、
半开连接泄漏）以及「P2P 切换」这条现有能力在 mux 下的行为，给出可判定的通过条件。
全部在 PR #3 合并前跑，结果写回本节。

| 编号 | 场景 | 判据 | 针对的风险 |
|---|---|---|---|
| **C1** | 2 MiB/s 限速链路上并发：1 条 512 MiB 文件传输（**限速到 1.2 MiB/s，留出 40% 余量**）+ 8 条小请求转发（4 KiB 请求/响应，每 100 ms 一次） | 转发请求 **P99 往返 ≤ 40 ms**，无一条超过 200 ms；文件传输完成且 SHA-256 一致。**对照组**：同样场景把 `RecordSize` 设成 512 KiB，P99 必须明显更差（≥ 2×）——这一条才是「32 KiB 有用」的证据 | 质疑 1：记录过大导致的队头阻塞 |
| **C2** | 慢读者：对端开一条流后完全停止读取，本端持续 `Write` 直到阻塞 | 本进程 RSS 增量 **≤ 50 MiB**（= 每方向 1 MiB 记录泵 + yamux 流窗口 + Go 堆余量），且 `Write` 在预算耗尽后阻塞而非继续吃内存；关掉慢读者后 `Write` 恢复并送达全部字节 | 质疑 4：内存无界。验证「有界阻塞」不是口号 |
| **C3** | 500 条半开连接：建立 500 条 TCP 连接到 `-L` listener，**不发送任何字节**（preamble 都没发），保持 30 s，然后全部 RST | ① 第 129 条起应被 `--max-pending=128` 立刻关掉（断言只收到 128 条被 accept）；② 剩下 128 条在 `--handshake-timeout=10s` 后被清理；③ 30 s 后 goroutine 回落到基线 ±10，RSS 增量 ≤ 20 MiB；④ 期间正常连接的 P99 建连延迟不受影响 | 质疑 4：半开连接泄漏。**两层防线分别断言**，不能只测一层 |
| **C4** | P2P 切换：`send` 传输 100 MiB 过程中，在第 30 MiB 处让 P2P 路径失效（`iptables -j DROP` 打掉 UDP，或直接 `Close` 直连 socket） | 传输**继续完成**（回退到 relay），最终 SHA-256 一致；或者——若走的是 mux 模式——`Switch` 返回错误、会话明确失败并给出可读原因，**不允许**静默丢字节。两种结果都算通过，第三种（卡住不动超过 60 s）算失败 | 质疑 2：切换语义。单流模式必须能救回来，mux 模式必须明确失败 |

C1 的实现提示与判据说明：

- 限速必须作用在**记录泵之下**（transport 层），否则测的是应用层而不是链路。
- yamux 的 `MaxStreamWindowSize` 要和记录泵预算一起调，否则慢链路上窗口会先把流
  卡住，测出来的不是队头阻塞。
- **为什么文件传输要限到 1.2 MiB/s 而不是打满 2 MiB/s**：在一条被单个大流打满的
  FIFO 字节流上，交互请求必然排在整队数据之后，此时 P99 由队列深度决定（1 MiB
  缓冲 = 500 ms），与记录大小无关。打满时任何记录大小都过不了 40 ms，那样的门槛
  不可判定。留 40% 余量后，排队主要由「一条记录占用链路多久」决定，正是本设计
  要优化的量。
- **打满时的真实行为要如实写进 README**：单连接里大文件会挤占交互流量，这不是
  记录大小的 bug，是单流 FIFO 的固有性质。要彻底解决得给控制流做优先级队列，
  属于阶段 2 之后（见 §9 的 X13）。

**这条替代了「32 KiB 一定够快」的直觉判断**：我们买的是「非饱和时的低延迟」和
「饱和时不会更差」，不是「饱和时也快」。

C4 的单流分支依赖现有的 `switchable` 包装，mux 分支依赖 PR #1 的锁路径。
**这两条要分别断言**，不能只测一个然后推断另一个。

## 7. 分步实施计划（按 PR 拆分）

原设计是一次交付、约 2,575 行生产代码。经评审，这个体量对阶段 1 偏重，
且把「基础转发」和「完整 ACL/限流」捆在一起，导致第一个 PR 就难以审查。

改为 **3 个独立可合并的 PR**，每个都能单独通过 CI 并保留可回退点：

### 7.1 为什么不是 800–1,200 行

评审估的是「yamux 包装 + 记录泵（60–120 行）+ `-L`/`-R` + 端口白名单 ≈ 800–1,200 行」。
逐项对一下，差额集中在三处，而且都不是抽象层：

| 评审的估算 | 实际 | 差额来源 |
|---|---|---|
| 记录泵 60–120 行 | **280 行** | 评审按「一条 `io.Copy` 加个信封」估的。实际要同时承担：① 引导期直通（hello/p2p 协商还在裸帧上跑）与复用期封 record 两种模式；② `FrameMuxStart` 双向屏障（两端都发出才能切，且屏障前到达的非 mux 帧要丢弃而不致命）；③ 两个方向各一个**有界阻塞**队列 + 单 drainer；④ 保留 `ErrTransportSwitched` 分支，否则 `--p2p` 升级会当场杀死会话。少任何一条都会破坏现有功能，不是可选装饰 |
| `internal/mux` 未单列 | **350 行** | 评审把「用 yamux」当成零成本。实际要有：`Config` 与内存预算换算（窗口 × 流数 ≤ 预算，否则 256 流 × 512 KiB 窗口 = 128 MiB 直接爆）、流数信号量、kind 路由、`CloseWrite` 映射、`LogOutput` 显式接管（yamux 默认写 `os.Stderr`，会污染 CLI 输出——研究二实测） |
| `peer.Session` 改造未计入 | **150 行**（含 `frameconn.go`） | 把 `conn`+`sec` 两个字段换成 `FrameConn` + `Pump`，是**改现有 1138 行文件的核心结构**，不是加个适配器。评审的 800–1,200 行里这一项是 0，但它恰恰是回归风险最高的地方（风险 X4） |

评审说对的部分已经照做：完整 ACL（CIDR + 主机名解析）从 PR #2 推到 PR #3，
`stream.go` 不再重复包一层（信号量与 kind 表并入 `mux.go`），`manager.go` 从 230
收到 150，`cli/forward.go` 去掉一层只为测试而存在的生命周期封装。
合计从 2,575 降到 **2,275**。

有一处我**没有**照做，而且是有意为之：评审把「限流」也归为越界功能。但
`MaxStreams` 限的是**并发**，不限**速率**——256 条流里每秒建/关上千次，
内网扫描的速率完全不受约束。§5.3 攻击场景 2 与 S8 都依赖这一层，
删掉它是拿安全性换行数。所以我保留限流，但把它从 100 行的四合一模块重写成
**60 行的手写令牌桶 + 三个超时字段**，不做配额/寿命/突发平滑那些阶段 1 用不到的机制。

**如果还要再砍，唯一的办法是把 `-R` 整个移出阶段 1**（省 540 行，降到约 1,735）。
但那等于承认「只能从本端拨出去，不能被拨进来」，而 `-R` 正是「让没有公网 IP 的
容器对外提供服务」这个核心场景。我不建议为了行数达标而砍掉它——**行数不是验收指标，
§6.5 的 C1–C4 与 §8 的决策门 G1 才是**。

### PR #1 —— 记录泵 + yamux 封装（940 生产 / 1,070 测试）

| 交付 | 内容 |
|---|---|
| `internal/peer/pump.go` | 记录泵：引导模式直通、复用模式封 record、`ErrTransportSwitched` 锁路径 |
| `internal/peer/frameconn.go` | `FrameConn` + `streamFrameConn` + `writeFull` |
| `internal/mux/config.go` | `Config`、默认值、内存预算换算 |
| `internal/mux/mux.go` | `Session` 包装、`Open`、`Serve`、流数信号量 |

**验收**：密文字节与旧路径**完全相同**（等价性单测）；`UseMux=false` 时行为逐字节不变；
39 项 e2e 全绿；`-race` 干净；文件传输 ≥ 485 MiB/s。

**风险**：这一步改 `peer.Session` 的内部结构，是整条路线最可能引入回归的地方。
单独提交，出问题时便于 bisect。

### PR #2 —— `-L` 本地转发 + 最小安全（795 生产 / 810 测试）

| 交付 | 内容 |
|---|---|
| `internal/wire/forward.go` | `Preamble`/`Status` 编解码、4 个新帧类型 |
| `internal/forward/pipe.go` | 双向 pipe + 半关闭 + EOF 后 5 s 强制拆除 |
| `internal/forward/local.go` | `-L`：listener 只建一次、每连接开流、前置头 |
| `internal/forward/allow.go` | **精简版 ACL**：仅「主机 + 端口精确匹配」，不做 CIDR、不做主机名解析 |
| `internal/cli/forward.go` | `cmesh forward -L` |

**验收**：10 并发 × 8 轮字节精确；拒绝时 dialer 未被调用；半关闭不挂起；
500 连接随机掐断后 goroutine 回落基线。

### PR #3 —— `-R` 反向转发 + 完整策略（540 生产 / 580 测试）

| 交付 | 内容 |
|---|---|
| `internal/forward/reverse.go` | `-R`：监听器 ID 匹配、重连不重复 bind |
| `internal/forward/limits.go` | 流数、建流速率（手写令牌桶）、空闲超时、会话寿命 |
| `internal/forward/allow.go`（扩） | CIDR、主机名启动时解析、`Permits` 返回 IP（封 TOCTOU） |
| `internal/cli/forward.go`（扩） | `--accept-reverse`、`--allow-listen-port`、启动横幅 |

**验收**：`-R` 无 `--allow-listen-port` 时零值拒绝；端口抢占场景被拒；
完整 e2e + 4 项混沌测试全绿。

### PR #2 实现记录（2026-10-21）

实际交付与设计的差异，以及原因：

| 设计 | 实现 | 原因 |
|---|---|---|
| `allow.go` 精简版约 90 行 | **约 300 行**（含 `NewAllowList` / `PortSet` / 解析） | 设计只写了「主机 + 端口精确匹配」，但匹配之前必须先有**解析并固定**的步骤，否则 DNS rebinding 的窗口就开着。`Permits` 返回 IP 这条从第一版就实现，测试 `TestAllowListPinsHostnameAtStartup` 直接验证「DNS 变了仍返回固定 IP」 |
| `limits.go` 60 行（限流 + 超时） | **约 190 行**（含 `pendingGate` / `Limits.normalise`） | 令牌桶本身 40 行，但 `max-pending` 需要一个非阻塞信号量，超时字段需要一套零值归一化。三者共享同一组默认值，拆开只会让默认值散落 |
| `pipe.go` 100 行 | **约 130 行** | 半关闭 + drain 超时 + 错误归一，实际就是这么长；没有可砍的抽象 |
| `local.go` 190 行 | **约 250 行** | 多了 `ParseSpec`（含 IPv6 方括号处理）与 `Serve` 的 pending gate 接入 |
| `manager.go` 150 行 | **约 290 行** | `Serve` 的 kind 分发、`Handle` 的四道闸、`dialApproved`（只拨已授权的 IP）、`reject`、`AddLocal`/`Close` 生命周期 |

**实测数据（PR #2）**：

| 项 | 结果 |
|---|---|
| 单测 | 31 个（wire 11 + forward 20），全过 |
| fuzz `FuzzReadPreamble` | 20 s / **417 万次执行**，无 panic、无 OOM |
| `-race` | 12/12 包全绿 |
| e2e `--quick` | **35/35**（`send`/`recv` 零回归） |
| e2e `--quick --large` | **37/37**（含 256 MiB） |
| 512 MiB 吞吐 | **527 / 585 / 624 / 624 MiB/s**，与 PR #1 持平 |
| `-L` 端到端 | 32 KiB 经真实 mux 转发到 echo，字节一致 |
| `--accept` 关闭 | 目标侧零字节到达，本地连接被关闭 |
| goroutine 基线 | 10 轮正常 EOF + 暴力断开后回落基线 ±2 |

**两处实现时发现的设计问题：**

1. **`net.Pipe` 不能用来测半关闭。** 它不实现 `CloseWrite`，所以基于它的测试走的是「整连接关闭」的降级路径，而生产路径（`*net.TCPConn` 目标、yamux 流）都支持半关闭。测试必须用真实 TCP，否则测的是错误的分支。
2. **`Permits` 不能对主机名重新解析。** 第一版在 `Permits` 里对请求的主机名再查一次 DNS 再比对，结果 DNS 一变就把**已经授权过的目标**拒掉了——这既是可用性故障，也说明 rebinding 防护的位置错了。正确做法是：配置过的主机名按**名字**匹配并直接返回启动时固定的 IP，只有请求方给了未配置的名字时才解析并比对。

### 相对初版砍掉/推迟了什么

| 初版设计 | 处理 | 理由 |
|---|---|---|
| `allow.go` 210 行（CIDR + 主机名解析 + TOCTOU 防护一次做完） | **拆开**：PR #2 只做主机+端口精确匹配（约 90 行），CIDR 与启动时解析留到 PR #3（补 40 行） | 阶段 1 的真实用法是「转发到固定的 db:5432」，CIDR 是运维便利而非必需；但 `Permits` 返回 IP 这个形状从第一版就定死，后面只加匹配规则、不改调用点 |
| `limits.go` 100 行（限流 + 超时 + 寿命 + 配额四套机制） | **保留限流，重写为 60 行**：一个手写令牌桶 + 三个超时字段 | 我一度想删掉它（`MaxStreams` 能防 OOM），但**这是错的**：`MaxStreams` 限的是并发，不限速率——攻击者可以在 256 条流里每秒建/关上千次，把内网扫描的速率放大到 `MaxStreams` 管不住的程度。§5.3 攻击场景 2 与 S8 都依赖这一层，删掉等于为凑行数削弱真实防线 |
| `stream.go` 130 行（信号量归还 + kind 表 + 流语义） | **并进 `mux.go`**，降到 70 行 | 三者都要读同一组字段，拆两个文件只是把锁的归属变模糊 |
| `manager.go` 230 行 | **收到 150 行** | 装配 + kind 分发 + 横幅，审计日志并入 `forward` 各模块而非集中一层 |
| `cli/forward.go` 340 行 | **收到 300 行** | 去掉一层只为测试而存在的生命周期封装，测试直接驱动 `Manager` |
| 审计日志独立成层 | **并入 PR #3 各模块** | 与安全策略同批审查更自然，且不需要单独的抽象 |

三个 PR 相加：940 + 795 + 540 = **2,275 生产**，1,070 + 810 + 580 = **2,460 测试**，
与 §1.2 的表逐行一致（`allow.go` 130/180 与 `cli/forward.go` 300/200 按精简版/
扩展版拆在 PR #2、#3 两侧）。
比初版少 13%。**PR #1 占了 40%，因为它是整条路线的地基**（记录泵 + 会话重构 +
mux 包装），后两个 PR 只是在这层地基上挂业务逻辑——这也是为什么 PR #1 必须单独
提交、单独 bisect。


## 8. 各 PR 内部的实施步骤

§7 决定「先做什么」，本节决定「一个 PR 内部按什么顺序写、每一步怎么自证」。
顺序的唯一约束是：**每一步结束时仓库都能构建、测试全绿、可回滚**。

### PR #1 内部

| 步 | 内容 | 自证 |
|---|---|---|
| 1.1 | `internal/peer/pump.go` 记录泵 + 引导直通 | `TestPumpBootstrapMatchesLegacy`：密文字节与改造前逐字节一致 |
| 1.2 | `session.go` 改为持有 `FrameConn`（此时无 mux，`boot` 传 nil） | `-race` 全绿 + 既有 e2e 39/39 + 512 MiB ≥ 485 MiB/s |
| 1.3 | `internal/mux` 包装 yamux | 单测：kind 路由、流 ID 奇偶、`MaxStreams`、`CloseWrite`、信号量归还、`LogOutput` |
| 1.4 | `NodeConfig.UseMux` 默认 false，`OnMux` 回调 | `UseMux=false` 时行为与今天逐字节相同；`UseMux=true` 对上旧版明确报错 |
| 1.5 | 在 mux 上跑完整文件传输（控制流承载 `peer.Session`） | 512 MiB SHA-256 一致 —— 证明控制面可用，这是 PR #2 的前提 |

**回滚点**：1.2 若引入回归，单独 revert 该提交即可回到旧 `conn`+`sec` 路径；
`UseMux` 默认 false 保证 1.3–1.5 在出问题时等于未启用。

### PR #2 内部

| 步 | 内容 | 自证 |
|---|---|---|
| 2.1 | `wire/forward.go`：preamble/status 编解码 + 严格解析 | 往返表驱动 + `FuzzReadPreamble` 2 分钟无 crash |
| 2.2 | `forward/pipe.go`：双向 pipe + 半关闭 + **EOF 后 5 s 强制拆除** | I2（HTTP/1.0）、I3（banner-first）、半关闭不挂起 |
| 2.2b | `--handshake-timeout`（10 s）落到 preamble 读 + `--max-pending`（128）落到 accept 侧 | C3（500 条半开 30 s 后 goroutine 回落基线） |
| 2.3 | `forward/allow.go` 精简版（主机 + 端口精确匹配） | 零值拒绝一切；`0.0.0.0/0` 构造期报错；拒绝时 dialer 调用次数为 0 |
| 2.4 | `forward/local.go` + `cli/forward.go`：`-L` 全链路 | I1（10 并发 × 8 轮字节精确）、I7、I8 |
| 2.5 | 启动横幅 + `--accept` 无 `--expect-sas` 时警告 | 横幅内容断言；S10 日志脱敏 |

### PR #3 内部

| 步 | 内容 | 自证 |
|---|---|---|
| 3.1 | `peer` 分发 `FrameForwardListen*`；`forward/reverse.go` | I6；多 `-R` 按 ID 匹配 |
| 3.2 | `--accept-reverse` + `--allow-listen-port`（零值拒绝） | I9、I10、S7（端口仍可被本地进程 bind） |
| 3.3 | `forward/limits.go`：流数 + 手写令牌桶（建流速率）+ 超时/寿命 | I11（第 257 条被拒）、S8（1000 次/秒 open 被限到 32/s）、单测断言桶的边界 |
| 3.4 | `allow.go` 扩展：CIDR + 启动时解析主机名 + `Permits` 返回 IP | S5（假 resolver 第二次返回攻击者 IP，断言仍拨第一次的） |
| 3.5 | 审计日志、`--deny-close`、§5.4 的 18 条逐条勾选 | S1–S10 全绿；P1–P9 实测值写回 §6.4 |

**决策门 G1（PR #3 合并前）**：P1 ≥ 485 MiB/s、P2 ≥ 200 MiB/s、P3 ≥ 500 MiB/s、
P6 < 80 MiB、§6.5 的混沌测试 C1–C4 全绿、既有 39 项 + 新增 e2e 全绿、`-race` 全绿。
不达标 → **不进入阶段 2**，先修回归。

**工期**：PR #1 约 3 天、PR #2 约 2.5 天、PR #3 约 2.5 天，合计约 8 个工程日（含缓冲）。

## 9. 主要风险

| 编号 | 风险 | 严重度 | 触发条件 | 缓解 |
|---|---|---|---|---|
| X1 | **开放代理**：宽松白名单 + code 泄露 | **高** | `--accept` + `10.0.0.0/8` + 任意端口 | 四层防御；零值拒绝；拒绝 `0.0.0.0/0`；`--deny-close`；审计；启动横幅 |
| X2 | **`-R` 端口抢占**：攻击者让受害容器 bind 任意 loopback 端口 | **高** | `--accept-reverse` 开着且没有端口白名单 | `--allow-listen-port` 零值拒绝；裁决权在暴露侧 |
| X3 | **记录泵死锁** | **高** | 读/写队列同时满 + 传输卡住 | 有界队列 + 单 drainer + `fail()` 关两端；`ConnectionWriteTimeout` 安全阀；P2/P9 压测 |
| X4 | **`peer.Session` 重构引入回归** | 中高 | PR #1 步 1.2 的改动遗漏 `ErrTransportSwitched` 或 `sendMu` | 密文等价性单测；`-race`；既有 e2e；P1 门槛；单独提交便于 bisect |
| X5 | **`StreamOpenTimeout` 关整个会话** | 中 | 单条流 SYN 未 ACK | 120 s；`Open` 前查 `NumStreams`；重连后 listener 存活 |
| X6 | **内存超预算** | 中 | 1 MiB 窗口 + 256 流 | 预算公式自动压低流数；P6 |
| X7 | **relay 消息数上升** | 中 | mux 启用 | 32 KiB 记录约 32 条/MiB，比研究二基线的 13 条高。这是换取低延迟下限的已知代价；P8 实测记录，若 relay 限流承压则在传输层做 sendmmsg 式批量 |
| X8 | **`-R` 目标被对端指定**（研究二原方案的漏洞） | 中 | 实现者照研究二写「Addr 仅用于日志」但忘了查表 | 结构性设计：Addr = 监听器 ID，只查表不拨号；S7 + 单测 |
| X9 | **白名单 TOCTOU / DNS rebinding** | 中 | 白名单写主机名 | `Permits` 返回 IP，拨号只用它；启动时固定；S5 |
| X10 | **yamux v0.x API 变动** | 低 | 升级 yamux | `internal/mux` 完全隔离；协议 spec 稳定；pin v0.1.2 |
| X11 | **新旧二进制互操作失败** | 低 | `forward` 对上旧 `send` | `caps` 协商 + 明确错误信息（I13） |
| X12 | **`-R` listener 跨重连泄漏** | 低 | 每次重连都注册 | 按 ID 幂等；会话结束清理；R-9 测试 |
| X13 | **单 `sendLoop` 在满载时抬高延迟** | 中 | 大文件打满链路 + 交互流量 | 单流 FIFO 的固有性质，不是记录大小的 bug。C1 只在留 40% 余量时验收低延迟；打满时的行为写进 README，控制流优先级队列留到阶段 2 |
| X14 | **队头阻塞没被 32 KiB 消除干净** | 中 | 慢链路 + 混合流量 | C1 直接验收：P99 往返 ≤ 40 ms；不达标就继续降记录或给控制流优先队列 |
| X15 | **慢读者把内存吃穿** | 中 | 对端开流不读 | C2：RSS 增量 ≤ 50 MiB；靠记录泵有界阻塞 + yamux 流窗口 |
| X16 | **半开连接耗尽 goroutine** | 中 | 只连不发 | C3：500 条半开 30 s 后回落到基线 ±10。两道防线缺一不可：`--max-pending=128` 挡住 accept 侧，`--handshake-timeout=10s` 挡住「拿到流但不发 preamble」 |
| X17 | **P2P 切换在 mux 下静默丢字节** | 高 | 传输中路径失效 | mux 模式锁路径，`Switch` 返回错误；C4 双分支分别断言 |

---

## 10. 对上游文档的修正清单

实现前必须先把这几条写回上游文档，否则实现者会照旧文档做出错误选择。

| # | 上游位置 | 原文 | 修正 | 依据 |
|---|---|---|---|---|
| 1 | `ARCHITECTURE-SDWAN.md` §A1、§6 阶段 1 | 「`wire.Version` 提升到 2，两端必须同时升级」 | **保持 1**，用 hello 的 `caps` 协商 | 帧格式未变，新帧类型是增量，`readPump` 的 default 分支忽略未知类型（`session.go:341-343`）；升版本会让 `send`/`recv` 在新旧二进制间不可用 |
| 2 | `ARCHITECTURE-SDWAN.md` §6 阶段 1 交付物 2（第 674 行） | 「新增 `FrameStreamData`（payload = 任意字节块，≤64 KiB）」 | **32 KiB，一条记录一次 transport 写** | 512 KiB 在 2 MiB/s 链路上有 250 ms 队头阻塞；AEAD 在 32 KiB 仍有 1408 MB/s。合并在实现时被否掉：它会把加密单元重新放大到 512 KiB `[实测]` |
| 3 | `ARCHITECTURE-SDWAN.md` §A9 预算表（第 604 行）与 §6 风险段（第 706 行） | 「记录泵缓冲 每方向 4 × 64 KiB」 | **每方向 1 MiB（32 条 × 32 KiB）** | 队列要有足够深度吸收一次 AEAD + 一次 Send 的抖动，又不能大到让死对端钉住内存。1 MiB 是 32 条记录；2 MiB 总量相对 128 MiB 的 yamux 预算可忽略 |
| 4 | `research/mux-and-portforward.md` §3.2 | `Preamble` 定义在 `internal/forward/proto.go` | 移到 `internal/wire/forward.go` | 状态码与帧 payload 都在 `wire`；放 `forward` 会形成 `forward→wire` 与 `wire→forward` 的环 |
| 5 | `research/mux-and-portforward.md` §3.4 | 5 层防御中只有第 1 层管「是否启用」，且 `-R` 与 `-L` 共用同一个 `--accept`（正文还写作 `--allow-forward`，与参数表不一致） | 拆出 `--accept-reverse` + `--allow-listen-port`（零值拒绝） | `-R` 让攻击者在受害容器内开监听端口，风险量级不同于 `-L` |
| 6 | `research/mux-and-portforward.md` §8 关键片段 3（第 974-1004 行）与第 961 行 | `Permits(addr)` 用主机名比对，然后 `net.DialTimeout("tcp", p.Addr)` 再解析一次 | `Permits` 返回**要拨的 IP**，拨号只用它 | check-then-dial TOCTOU / DNS rebinding |
| 7 | `research/mux-and-portforward.md` §3.3 时序图（第 443 行）与 §3.4 末条（第 507 行） | 「`-R` 时 preamble 的 `Addr` 仅用于日志/审计」 | `Addr` 改为**监听器 ID**，用于查本地配置 | 多条 `-R` 时无法区分是哪条监听器触发；ID 只查表不拨号，保留「目标来自本地配置」的结构性保证 |
| 8 | `research/mux-and-portforward.md` §2.5 | `messageStream` 适配器（mux 包在 `MessageConn` 上） | 不实现；由记录泵承担消息↔字节流的转换，且位于 secure **之上** | secure 的严格递增计数器要求单读者，mux 在 secure 之下会让多流解密看到乱序计数器 |

---

## 11. 附：一页纸的决策摘要

| 决策 | 选择 | 性质 |
|---|---|---|
| 复用库 | `hashicorp/yamux v0.1.2`（+33 KB，零传递依赖，MPL-2.0） | 必须（研究二已否决 smux/QUIC 多流） |
| 分层 | `transport → secure → 记录泵 → yamux → 业务流` | 必须 |
| `secure.go` | 不改 | 必须 |
| 记录大小 | **32 KiB**，一条记录一次 transport 写 | 必须 |
| 记录泵缓冲 | 每方向 1 MiB（32 条 32 KiB 记录），有界阻塞 | 必须 |
| 写入合并 | **不做**：会把加密单元放大回 512 KiB，重新引入队头阻塞 | 必须（明确不做） |
| `wire.Version` | 保持 1 + `caps` 协商 | 必须 |
| 控制流 | yamux stream，`kind=0`，initiator 打开，只接受一次 | 必须 |
| 流 kind | 0=控制，1=转发 | 必须 |
| `-L` 默认 bind | `127.0.0.1` | 必须 |
| `-R` 默认 bind | `127.0.0.1`（请求），由对端裁决 | 必须 |
| `-R` 授权 | `--accept-reverse` + `--allow-listen-port`（零值拒绝） | 必须 |
| 白名单 | 零值拒绝一切；启动时固定 IP；`Permits` 返回 IP | 必须 |
| `0.0.0.0/0`、`::/0` | 启动失败 | 必须 |
| 状态码 | 0 OK / 1 Denied / 2 DialFail / 3 TooMany / 4 BadRequest / 5 BindFail | 必须 |
| 策略与拨号顺序 | 先策略后拨号 | 必须 |
| `MaxStreams` | 256，被 128 MiB 预算自动压低 | 必须 |
| `MaxStreamsPerSec` | 32，手写令牌桶（不限并发、只限建流速率） | 必须 |
| `StreamOpenTimeout` | 120 s | 必须 |
| `StreamCloseTimeout` | 60 s | 必须 |
| `yamux.LogOutput` | 显式设为 slog 适配器或 `io.Discard` | 必须 |
| 传输切换 | 只在 mux 建立前（A8）；mux 起来后 `Switch` 返回错误 | 必须 |
| `--pipe-drain` | 5 s：一侧 EOF 后给另一侧 5 s 送完，超时 `Reset()` | 必须 |
| `--handshake-timeout` | 10 s：流建立后读完前置头 + 状态交换的总预算 | 必须 |
| 重连 | listener 只建一次；所有流断开；不做流级续传（A11） | 必须 |
| 文件共存 | 控制流承载完整 `peer.Session`；`--also-recv-files` 免费获得 | 必须 |
| `forward` 发文件 | v1 不做 | 可选 |
| `--allow-any` | 永远不做 | 必须 |
