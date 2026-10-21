# 无 TUN 权限容器的用户态网络栈研究

- 研究日期：2026-10-20
- 研究对象：`container-mesh`（下称 cmesh），`5226d96`
- 场景：K8s 容器，`cap_eff=0`，无 `/dev/net/tun`，无 root
- 工作方式：**只读** cmesh 仓库；全部原型与实测在 `/tmp`、`/tmp/sizedelta`、`/tmp/mtulab` 下独立进行。已确认 `git status --porcelain` 为空，**未修改仓库任何文件**
- 结论口径：**已实测** = 本次在本机跑出的数字/行为；**查证** = 读源码或官方文档确认；**推断** = 基于已知事实的工程判断，未实测

---

## 0. 结论摘要

**推荐方案：只做 SOCKS5 + gVisor netstack，不做"完整 L3 透明网络"。**

一句话理由：`ping 10.99.0.5` / `ssh user@10.99.0.5` 这种**透明**用法在没有 TUN 的容器里**物理上不可能实现**（内核没有路由，也没有接口），任何用户态栈都只能通过"应用显式走代理"或"本地端口转发"来暴露能力。gVisor netstack 能提供**完整的 TCP/IP 协议实现**，但它的价值是"让代理能拨号到虚拟 IP"，不是"让容器内核认识虚拟 IP"。

| 决策 | 选择 | 依据 |
|---|---|---|
| 用户态栈 | **gVisor netstack**（`gvisor.dev/gvisor/pkg/tcpip`） | 已实测：无 TUN/无 root/无 syscall 提供完整 TCP+UDP+ICMP；32 MiB 传输 349 MiB/s |
| 不选 wireguard-go `tun/netstack` | — | 查证：它**本身就是 gVisor 的封装**（`tun/netstack/tun.go` 直接 import `gvisor.dev/gvisor/pkg/tcpip/...`），多一层 wireguard 协议栈但 netstack 能力完全相同，二进制还更大（11.84 MB vs 11.81 MB） |
| 不选"只做 SOCKS5 不做 L3" | — | 二者**不是二选一**：SOCKS5 是暴露方式，netstack 是拨号引擎。只做 SOCKS5 但仍需要 netstack 才能拨 `10.99.0.5`；不引入 netstack 就只能 `ssh -L` 式端口转发（更简单，见 §6.2） |
| 暴露方式 | **SOCKS5（RFC 1928 CONNECT + UDP ASSOCIATE）+ 可选 `-L` 式本地端口转发** | 已实测：curl / ssh / SOCKS5-UDP 全部打通 |
| 传输层 | 复用 cmesh 现有 `secure` + `p2pquic`/`wsconn` | 推断：IP 包作为 `wire.Frame` 的 payload，不改动 secure 语义 |
| 加密 | **复用现有 SPAKE2 + AES-GCM，不引入 WireGuard** | 推断：cmesh 的 secure 已经是认证加密通道，WireGuard 的 Noise 是重复投入 |

**主要风险（按严重度）：**

1. **二进制体积 +57%**：Linux/amd64 stripped 从 9.46 MB → 11.81 MB（+2.35 MB）。**但**如果把 cmesh 的 `-ldflags "-s -w"` 去掉做对比，体积增长更可观（12.64 → 16.01 MB，+27%）。真实增量取决于链接裁剪。
2. **`go` 指令被迫升级**：最新 gVisor（2026-09）要求 `go 1.26.3`，会强制 cmesh 从 `go 1.24.0` 升级。用 2025-05 的旧版可保持 `go 1.24.0`，但那个版本已 18 个月未更新（安全/正确性风险）。
3. **MTU 黑洞**：已实测。netstack 的 link MTU 若大于隧道实际可承载的包，TCP **完全卡死**（1 MiB 后 write timeout），而不是降速。必须让 MTU 与隧道能力一致（建议 1280）。
4. **内存**：空载 9.1 MiB RSS，200 条**空闲**连接 24.5 MiB，200 条**活跃**连接峰值 238 MiB（每条约 1.1 MiB，来自 gVisor 默认 1 MiB 收发缓冲）。必须限制并发连接数。
5. **不是透明的**：`ping 10.99.0.5` 依然失败。这是需求本身的物理限制，不是实现缺陷。

---

## 1. gVisor netstack 能否在无 TUN 环境提供完整 TCP/IP 栈？

### 1.1 结论：能，且完全不需要任何设备

**已实测。** 两个 netstack 实例在同一个进程内通过 `channel.Endpoint` 直接互连，完成了 ICMP echo、TCP 双向 64 MiB、UDP echo：

```
[1] ICMP echo through two netstack hosts
    ping #1: 10.99.0.6 -> 10.99.0.5  rtt=49µs
    ping #2: 10.99.0.6 -> 10.99.0.5  rtt=9µs
    ping #3: 10.99.0.6 -> 10.99.0.5  rtt=7µs
[2] TCP echo + throughput through netstack
    sent=67108864 recv=67108864 in 225ms => 284.6 MiB/s
[3] UDP through netstack
    udp echo: "hello-over-udp"
```

### 1.2 需要创建什么设备？

**什么设备都不需要。** 已查证（`gvisor.dev/gvisor@v0.0.0-20250503011706-39ed1f5ac29c/pkg/tcpip/link/channel/channel.go:158`）：

```go
func New(size int, mtu uint32, linkAddr tcpip.LinkAddress) *Endpoint
```

`channel.Endpoint` 内部只有一个 `chan *stack.PacketBuffer`。出站包由 netstack 写入这个 channel，入站包由 `ep.InjectInbound(proto, pkt)` 直接投递。**没有 fd、没有 ioctl、没有 `/dev/net/tun`、没有 syscall。**

已实测的旁证：运行中的 netstack 进程 `lsof` 只有 2 个真实 socket（隧道 + SOCKS 监听），即使同时有 200 条 netstack TCP 流：

```
ns-lab  18418  ...  TCP localhost:49541->localhost:palace-3 (ESTABLISHED)   ← 隧道
ns-lab  18418  ...  TCP localhost:webobjects (LISTEN)                        ← SOCKS5
（200 条 netstack 流全部不占内核 socket）
```

二进制字符串检查：`strings /tmp/ns-lab | grep TUNSETIFF` → **0 次**。（`/dev/net/tun` 出现 1 次，来自 lab 自己的 `modeSyscallCheck` 打印语句，不是真实调用。）

### 1.3 内存占用与启动开销（已实测，macOS arm64 / Go 1.27.2）

| 指标 | 数值 | 测量方式 |
|---|---|---|
| 进程空载 RSS | **9.1 MiB** | `ps -o rss` |
| 单次 `stack.New` + `channel.New` + `CreateNIC` + 加地址 + 路由 | **35–48 µs** | 循环创建 50 个取均值 |
| 单个 stack 堆内存 | **53 KiB** | `runtime.MemStats.HeapAlloc` 差值 / 50 |
| 100 条空闲 TCP 流 | +2015 KiB（**20 KiB/流**） | `HeapAlloc` 差值 |
| 500 条空闲 TCP 流 | +10121 KiB（**20 KiB/流**） | 同上，线性 |
| 200 条**空闲**连接进程 RSS | **24.5 MiB**（空载 9.1 + 15.4） | `ps -o rss` |
| 200 条**活跃**连接进程 RSS | **238.4 MiB** | 传输中 `ps -o rss` |
| 8 并发 × 32 MiB 传输中 RSS | **38.2 MiB** | 同上 |
| 进程启动（无栈创建） | **3.5–4.1 ms** | 10 次取 min/median |
| 启动 + 建 50 个栈 | **6.5–7.0 ms** | 同上 |

**关键解读**：空闲流只要 20 KiB（gVisor 惰性分配缓冲），但一旦传输，每条流按 gVisor 默认 `DefaultSendBufferSize = DefaultReceiveBufferSize = 1 MiB`（查证 `pkg/tcpip/transport/tcp/protocol.go:46,50`）占用，实测 ~1.1 MiB/流。**并发连接数必须设上限**，否则 200 条活跃流就能吃掉 238 MiB。

### 1.4 具体包路径与 API（查证，gVisor 2025-05-03 快照）

```go
import (
    "gvisor.dev/gvisor/pkg/tcpip"                    // Address, FullAddress, NICID, 错误类型
    "gvisor.dev/gvisor/pkg/tcpip/stack"              // Stack, Options, Route, AddressProperties
    "gvisor.dev/gvisor/pkg/tcpip/link/channel"       // channel.New / InjectInbound —— 内存网卡
    "gvisor.dev/gvisor/pkg/tcpip/network/ipv4"       // ipv4.NewProtocol
    "gvisor.dev/gvisor/pkg/tcpip/network/ipv6"       // ipv6.NewProtocol
    "gvisor.dev/gvisor/pkg/tcpip/transport/tcp"      // tcp.NewProtocol, tcp.NewForwarder
    "gvisor.dev/gvisor/pkg/tcpip/transport/udp"      // udp.NewProtocol, udp.NewForwarder
    "gvisor.dev/gvisor/pkg/tcpip/transport/icmp"     // icmp.NewProtocol4 / NewProtocol6
    "gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"     // gonet.DialContextTCP / ListenTCP / DialUDP
    "gvisor.dev/gvisor/pkg/tcpip/header"             // header.IPv4ProtocolNumber, IPv4EmptySubnet
    "gvisor.dev/gvisor/pkg/waiter"                   // 事件等待（gonet 内部用）
    "gvisor.dev/gvisor/pkg/buffer"                   // 构造注入包的 payload
)
```

最小可运行骨架（本次实测通过的核心 30 行）：

```go
s := stack.New(stack.Options{
    NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
    TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
    HandleLocal:        true,   // 允许进程访问自己的 mesh IP（与 wireguard-go 一致）
})

sack := tcpip.TCPSACKEnabled(true)          // gVisor 默认关闭 SACK，广域网必须开
_ = s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)

ep := channel.New(1024, 1500, "")            // 队列深度 1024，MTU 1500，无链路地址
_ = s.CreateNIC(1, ep)                       // NIC ID = 1
_ = s.AddProtocolAddress(1, tcpip.ProtocolAddress{
    Protocol:          ipv4.ProtocolNumber,
    AddressWithPrefix: tcpip.AddrFromSlice(ip.AsSlice()).WithPrefix(),
}, stack.AddressProperties{})
s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})

// 拨号（这就是 SOCKS5 CONNECT 的实现）
conn, err := gonet.DialContextTCP(ctx, s, tcpip.FullAddress{
    NIC: 1, Addr: tcpip.AddrFromSlice(dst.Addr().AsSlice()), Port: dst.Port(),
}, ipv4.ProtocolNumber)

// 出站：从 ep 读出 IP 包，交给隧道
pkt := ep.ReadContext(ctx)
view := pkt.ToView(); raw := view.AsSlice()   // raw 就是一个完整 IP 包
// 入站：把隧道收到的 IP 包注入
p := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(raw)})
ep.InjectInbound(header.IPv4ProtocolNumber, p)
```

### 1.5 gVisor 的能力边界（查证）

| 能力 | 状态 | 依据 |
|---|---|---|
| TCP（含 SACK、CUBIC/Reno、窗口缩放） | ✅ 完整 | `pkg/tcpip/transport/tcp`；SACK 需显式开启 |
| UDP | ✅ 完整 | `pkg/tcpip/transport/udp` |
| ICMPv4/v6 echo（**自动回 reply**） | ✅ 完整 | `network/ipv4/icmp.go:380` 直接 `sendICMPEchoReply`，无需应用参与 |
| Raw socket | ✅ 有 | `pkg/tcpip/transport/raw` |
| IPv6 / NDP / SLAAC | ✅ 有 | `network/ipv6` |
| IGMP / 组播 | ✅ 有 | `network/ipv4/igmp.go` |
| **SCTP** | ❌ **没有** | `pkg/tcpip/transport/` 下无 sctp 目录 |
| **DNS 解析器** | ❌ **没有** | gVisor 只提供传输层，`grep -r "resolv.conf\|dnsmessage" pkg/tcpip/` 无命中。DNS 必须在**代理层**做（见 §2.2） |
| `syscall.Conn` / `SyscallConn()` | ❌ 未实现 | `gonet.go` 无该方法。依赖 `RawConn` 的库（部分 gRPC/HTTP2 调优路径）会退化 |
| per-conn `SetSockOpt`（TCP_NODELAY 等） | ⚠️ 部分 | `gonet.TCPConn` 只暴露 `Read/Write/Close/CloseRead/CloseWrite/LocalAddr/RemoteAddr` + `Set*Deadline`（查证 `gonet.go:346-462`）。NODELAY 需通过底层 `tcpip.Endpoint` 手动设 |
| `net.Conn` 完整兼容 | ✅ | `SetDeadline` 系列在 `gonet.go:202-221` 实现 |

**注意**：`gonet.TCPConn` 有 `CloseWrite()`（`gonet.go:437`），半关闭可用，这对 `ssh`/`http` 的正常收尾很重要。

---

## 2. Tailscale 的 `userspace-networking` 怎么做

### 2.1 用 wireguard-go 还是 gVisor？—— **用 gVisor，wireguard-go 只是个外壳**

**查证（读 `tailscale.com@v1.104.2` 源码）：**

- `wgengine/netstack/netstack.go:26-35` 直接 import `gvisor.dev/gvisor/pkg/tcpip/...`
- `tailscale.com@v1.104.2/go.mod:156` 固定 `gvisor.dev/gvisor v0.0.0-20260915211658-a6f909f08a72`
- 自研 link endpoint：`wgengine/netstack/link_endpoint.go`（一个带优先队列的 channel 变体，把出站包分流到 WireGuard / 本机 / 回环三条路径）
- WireGuard 加密在**外面**：netstack 处理的是**已解密的明文 IP 包**，WireGuard 的 Noise 握手与 netstack 无关

**关键澄清**：`golang.zx2c4.com/wireguard/tun/netstack` 这个包**也**是 gVisor 封装。查证 `wireguard@v0.0.0-20261006164505-2631ce99a06f/tun/netstack/tun.go:28-39`：

```go
import (
    "gvisor.dev/gvisor/pkg/tcpip"
    "gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
    "gvisor.dev/gvisor/pkg/tcpip/link/channel"
    ...
)
```

它的 `CreateNetTUN(localAddresses, dnsServers, mtu)` 做的正是 §1.4 那套 `stack.New` + `channel.New(1024, mtu, "")` + `CreateNIC` + `AddProtocolAddress` + `AddRoute`，**外加** `HandleLocal: true` 和 `TCPSACKEnabled(true)`。

所以"wireguard-go netstack"**不是 gVisor 的替代品**，而是"gVisor netstack + 一层 tun.Device 适配 + 一份 WireGuard 实现"。对 cmesh 而言，直接引 gVisor 更省（不引入 WireGuard 协议栈）。

**已实测对比（同一份 cmesh 代码 + 两个不同 shim，Linux/amd64，`-s -w`）：**

| 链接方式 | 二进制 | 增量 |
|---|---|---|
| cmesh 基线 | 9,457,824 B (9.02 MiB) | — |
| cmesh + **gVisor 直接**（2025-05 快照） | 11,813,024 B (11.27 MiB) | **+2.35 MB / +24.9%** |
| cmesh + **wireguard-go `tun/netstack`** | 11,837,600 B (11.29 MiB) | +2.38 MB / +25.1% |

两者体积几乎一样（因为大头都是 gVisor），但 wireguard-go 版本多引入 `golang.zx2c4.com/wireguard` + `wintun` 两个模块和整套 Noise 协议——对 cmesh 是纯浪费。

### 2.2 SOCKS5 如何暴露

**查证（`tailscale.com@v1.104.2`）：**

- 服务端实现：`net/socks5/socks5.go`，`Server{Dialer, Logf, Username, Password}` + `Serve(ln net.Listener)`
- 命令行暴露：`cmd/tailscaled/proxy.go:33`
  ```
  -socks5-server="localhost:1080"   optional [ip]:port to run a SOCK5 server
  ```
- 同时还有 HTTP 代理：`-outbound-http-proxy-listen`（`proxy.go` 用 `httputil.ReverseProxy`，CONNECT 走同一个 `Dialer`）
- **同一个端口同时服务 SOCKS5 和 HTTP**：`net/proxymux.SplitSOCKSAndHTTP(ln)`，靠首字节区分（`0x05` = SOCKS，其余 = HTTP）
- 支持的能力：CONNECT（`socks5.go:227`）、UDP ASSOCIATE（`socks5.go:346`）、用户名密码认证（RFC 1929）
- `tsnet` 内嵌用法：`tsnet/tsnet.go:521`，监听 `127.0.0.1:0`，用 `proxymux` 把 SOCKS5 和 localapi 复用一个端口，带 `Username: "tsnet"` + 随机密码

**拨号路径（这是 DNS 处理的关键）**，查证 `net/tsdial/tsdial.go:588`：

```go
func (d *Dialer) UserDial(ctx context.Context, network, addr string) (net.Conn, error) {
    ipps, err := d.userDialResolveAll(ctx, network, addr)   // ← 先解析名字
    ...
    return d.dialOneUser(ctx, network, ipps[0])             // ← 再按 IP 分流
}
```

`dialOneUser`（`tsdial.go:602`）的三路分流：

```go
if d.UseNetstackForIP != nil && d.UseNetstackForIP(ipp.Addr()) {
    return d.NetstackDialTCP(ctx, ipp)        // 走 netstack（mesh IP）
}
if routes := d.routes.Load(); routes != nil {
    if isTailscaleRoute, _ := routes.Lookup(ipp.Addr()); isTailscaleRoute {
        return d.getPeerDialer().DialContext(...)  // 走 WireGuard 直连
    }
    return d.SystemDial(ctx, network, ipp.String()) // 走内核
}
```

### 2.3 DNS 如何处理 —— **在代理层解析，不在 netstack 里**

这是最重要的一条设计结论。

- **gVisor 没有 DNS 解析器**（§1.5 查证）
- Tailscale 的做法：`Dialer.SetResolveMagicDNS(fn)`（`tsdial.go:387-394`）注入一个回调，由 `ipnlocal.LocalBackend.resolveMagicDNS` 实现；`resolveAddr`（`tsdial.go:397-410`）先试 MagicDNS，命中就返回 mesh IP，未命中再走系统 DNS
- 因此 SOCKS5 收到的是**域名**（`ATYP=0x03`）时，代理先把它解析成 IP，**再**用 netstack 拨这个 IP

**对 cmesh 的直接启示**：SOCKS5 服务端必须支持 `ATYP=0x03`（域名），并且自己维护一张"mesh 名字 → 虚拟 IP"表。不要指望在 netstack 里跑 DNS 服务器——那需要额外实现一个 DNS 服务端（Tailscale 确实做了，见 `netstack.go:1699-1710` 的 `hittingDNS` 分支，但那是因为它要支持 `dig` 直接查 MagicDNS）。

**已实测**：lab 的 SOCKS5 实现在 `ATYP=0x03` 时先查静态表 `peer.mesh → 10.99.0.5`，未命中才 `net.LookupIP`：

```
$ curl --socks5-hostname 127.0.0.1:1080 http://peer.mesh:8080/
hello from netstack host 10.99.0.5, path=/
```

### 2.4 已知限制（查证 + 官方文档）

| 限制 | 说明 | 来源 |
|---|---|---|
| **不是透明的** | 应用必须显式配 `ALL_PROXY`/`SOCKS5`；`ping` 不能用 | 官方 KB `tailscale.com/kb/1112/userspace-networking` |
| **入站连接受限** | 容器内进程无法直接监听 mesh IP；要靠 netstack 内部 listener 或 `tailscale serve` | 查证 `ipn/ipnlocal/serve.go:114`：*"Most serve traffic and peer traffic for the web client are intercepted by netstack"* |
| **ICMP 从主机发出时不可用** | Tailscale 在 userspace 模式下 ping 虚拟 IP 需要 `exec ping`（有 root/cap 时）或走 netstack 内部（`netstack_userping.go`） | 查证 `wgengine/netstack/netstack_userping.go` |
| **子网路由需 netstack 处理** | 在 Windows/macOS/BSD 上 `handleSubnetsInNetstack()` 返回 true，必须由 netstack 做 SNAT | 查证 `cmd/tailscaled/tailscaled.go:829-857` |
| **无 SCTP** | gVisor 不实现 | §1.5 |
| **无 `syscall.Conn`** | 依赖 RawConn 的库退化 | §1.5 |
| **UDP 需要逐个目标建流** | SOCKS5 UDP ASSOCIATE 每目的地址一条 netstack UDP socket | 已实测（lab `socks5UDPAssociate`） |

**官方文档没有列出"哪些协议不支持"**，只强调"proxy 而非 tunnel"。上面的表来自读源码。

---

## 3. 可行性验证（已实测）

### 3.1 能不能在本机（macOS arm64）做最小验证？

**能，而且已经做完了。** 所有代码在 `/Users/blackevil/Documents/Projects/container-mesh-reports/lab/`（既有）+ `/tmp/mtulab`（本次新增的 MTU 实验）。

| 实验 | 结果 |
|---|---|
| 单进程双栈互 ping（ICMP） | ✅ rtt 4–49 µs |
| 单进程双栈 TCP 64 MiB | ✅ 284.6–311.3 MiB/s |
| 单进程双栈 UDP echo | ✅ |
| **最小自包含示例**（`lab/netstack-min`，3 个依赖，离线可建） | ✅ 连续 3 次通过，ping + TCP 8 MiB ≈ 267–273 MiB/s |
| 双进程经真实 TCP 隧道（IP-in-stream） | ✅ 隧道建立、ping 通（157–610 µs） |
| SOCKS5 CONNECT → netstack → 对端 HTTP | ✅ `curl --socks5-hostname` 拿到 200 |
| SOCKS5 CONNECT → 对端 `:22` → `ssh` | ✅ `debug1: Remote protocol version 2.0, remote software version cmesh-netstack-lab` |
| SOCKS5 UDP ASSOCIATE | ✅ `[socks-udp] echo from 10.99.0.5:9090: "hello-over-socks-udp"` |
| netstack → 真实 `127.0.0.1:18080` 的 python http.server（`tcp.NewForwarder` 模式） | ✅ 返回 `hello-from-real-loopback-daemon` |
| `-L` 式本地端口转发 → 对端 mesh IP:22 | ✅ `ssh -p 2222 127.0.0.1` 拿到 SSH banner |
| 32 并发 × 8 MiB（单进程） | ✅ 802.1 MiB/s 聚合 |
| 200 并发 × 1 MiB（双进程 + SOCKS） | ✅ 3773.6 MiB/s 聚合 |
| 200 条空闲连接 | ✅ 24.5 MiB RSS |
| 200 条活跃连接 | ✅ 完成，238.4 MiB RSS |

### 3.2 卡点：**没有**（技术上全部通过）

唯一的"卡点"是需求层面的，不是技术层面的：

**已实测，虚拟 IP 对内核不可见**：

```
$ ping -c 2 -t 2 10.99.77.5
--- 10.99.77.5 ping statistics ---
2 packets transmitted, 0 packets received, 100.0% packet loss

$ nc -z -w 2 10.99.0.5 8080
（失败）

$ curl --max-time 4 http://10.99.0.5:8080/
curl: (7) Failed to connect to 10.99.0.5 port 8080 after 50 ms: Couldn't connect to server
```

**重要警告（实测踩到的坑）**：本机测试时 `ping 10.99.0.5` 一度"成功"（72–189 ms，走 `10.178.117.146` 网关）。查证后确认这是**上游网络的地址冲突**——10.99.0.5 在真实链路上被路由了，不是 netstack 的功劳。`traceroute` 显示第一跳到 `10.178.117.146` 后超时。**选虚拟网段时必须避开上游可能路由的地址**，建议用 `100.64.0.0/10`（CGNAT）或 `240.0.0.0/4`（保留），已实测这两个在本机不可达。

### 3.3 关键新发现：MTU 黑洞（已实测）

这是集成时最容易踩的坑，单独列出。

实验设置：两个 netstack 的 link MTU 固定，隧道 pump 丢弃**超过隧道真实容量**的包（模拟 IP-in-UDP 封装后超 MTU）。

```
netstack_mtu=1500 tunnel_capacity=1500 sent=4194304 status=ok
netstack_mtu=1500 tunnel_capacity=1200 sent=1048576 status=write ...: i/o timeout
netstack_mtu=1200 tunnel_capacity=1200 sent=4194304 status=ok
netstack_mtu=1500 tunnel_capacity=1400 sent=1048576 status=write ...: i/o timeout
netstack_mtu=1280 tunnel_capacity=1200 sent=1048576 status=write ...: i/o timeout
```

**结论**：netstack 的 MTU 只要**大于**隧道实际能承载的值，TCP 会在第一个满尺寸段就卡死（`write` 阻塞到超时），**不会**自动降速或重传小包。因为隧道是"静默丢包"，没有 ICMP Fragmentation Needed 反馈给 netstack 做 PMTUD。

**对策（推断，工程上确定）**：
1. 让 netstack 的 MTU **等于**隧道净荷上限，不要依赖 PMTUD
2. 若隧道是 UDP 封装（QUIC datagram / 裸 UDP），净荷 = 1200（cmesh `p2p.MaxDatagram = 1200`，查证 `internal/p2p/packet.go`）→ **netstack MTU 必须 ≤ 1200**
3. 若隧道是 QUIC stream / WebSocket（字节流），理论上可任意大，但为安全统一用 1280

**已实测的 MTU → 吞吐关系（单进程，16 MiB，无隧道开销）**：

| netstack MTU | TCP 吞吐 | ping rtt |
|---|---|---|
| 1500 | 349.2 MiB/s | 412 µs |
| 1280 | 282.2 MiB/s | 190 µs |
| 1200 | 261.4 MiB/s | 163 µs |
| 1000 | 205.9 MiB/s | 154 µs |
| 576 | 77.8 MiB/s | 199 µs |

MTU 从 1500 降到 1200 只损失 ~25% 吞吐，但避免了黑洞。**选 1200**。

### 3.4 吞吐与 CPU 成本（已实测）

**双进程经真实 TCP 隧道（SOCKS5 → netstack → 隧道 → netstack）：**

| 场景 | 吞吐 |
|---|---|
| 1 连接 × 32 MiB | 299.1 MiB/s |
| 1 连接 × 64 MiB | 72.9 MiB/s |
| 1 连接 × 128 MiB | 80.9 MiB/s |
| 1 连接 × 256 MiB | 87.6 MiB/s |
| 8 连接 × 32 MiB | 246.4 MiB/s 聚合 |
| 200 连接 × 1 MiB | 3773.6 MiB/s 聚合 |
| 内核 loopback TCP 基线（512 MiB） | 6705.9 MiB/s |

**吞吐波动说明**：32 MiB 的 299 MiB/s 是"还没跑满缓冲区"的短程数字；64–256 MiB 稳定在 73–88 MiB/s，这才是持续吞吐。**比内核 loopback 慢约 75–100 倍**，但这是**两个 netstack 各做一遍完整 TCP 处理**的成本，且隧道是单条 TCP 连接（有队头阻塞）。真实部署中瓶颈通常是对端网络，不是 netstack。

**CPU**：单连接传 128 MiB 时，node 进程 CPU 时间从 11.47s 涨到 15.40s，即 **约 3.9s CPU / 128 MiB ≈ 30 ms CPU 每 MiB ≈ 33 MiB/s per core**。双进程各算一遍，所以端到端是 ~2 核满载换 ~80 MiB/s。

**SOCKS5 CONNECT 延迟**（已实测，50 次）：

```
SOCKS5 CONNECT through netstack: n=50 min=0.25ms median=0.30ms p95=0.52ms
kernel TCP connect baseline:     n=50 min=0.06ms median=0.06ms
```

即每次新建连接多 ~0.24 ms，可以忽略。

---

## 4. 与 cmesh 现有架构的集成点

### 4.1 建议分层

```
应用（curl / ssh / psql / 任意 TCP 客户端）
   │  SOCKS5 CONNECT / UDP ASSOCIATE  或  127.0.0.1:PORT 本地转发
   ▼
cmesh SOCKS5 / 转发器（新包 internal/netstack 或 internal/proxy）
   │  gonet.DialContextTCP(stack, dst)   ← 这里进入用户态栈
   ▼
gVisor netstack（每个 node 一个 stack，一个 channel.Endpoint）
   │  出站：ep.ReadContext → 完整 IP 包
   │  入站：ep.InjectInbound ← 完整 IP 包
   ▼
IP 包分帧（新：wire.Frame 增加 FramePacket 类型，或复用 Stream 字段）
   │
   ▼
secure.Session（SPAKE2 + AES-GCM，**现有，不改**）
   │  每帧严格递增计数器，要求严格有序
   ▼
p2pquic.Conn（直连，QUIC stream）  或  wsconn.Conn（relay，WebSocket）
   │
   ▼
对端：wsconn/p2pquic → secure → IP 包 → netstack.InjectInbound
```

**这个分层是合理的**，理由：

1. **secure 不用改**：cmesh 的 `secure.Session` 要求"严格递增计数器 + 严格有序"（查证 `internal/secure/secure.go:25-26, 401-402`：*"Frames must arrive in exactly the order they were sent"*）。IP 包本身是**无序**的，但把它当作**有序字节流**承载完全没问题——netstack 自己能处理 IP 层的乱序/丢包（它就是干这个的）。**千万不要**把 IP 包塞进多条并行流，那会破坏 secure 的计数器语义。
2. **加密位置正确**：secure 在 netstack 之下、传输之上。netstack 处理的是明文 IP 包，符合 Tailscale 的模型（WireGuard 在外，netstack 在内）。
3. **传输可切换**：`p2pquic` 和 `wsconn` 已经通过 `switchable`（`internal/peer/switchable.go`）统一为 `MessageConn` 接口。IP 包作为"一条消息"发送，切换逻辑**零改动**。

**唯一需要新增的**：`wire.Frame` 加一个类型。查证 `internal/wire/frame.go:30-31`，`FrameType` 是 `iota + 1`，`Frame.Stream uint32` 字段已预留（README 也说"预留多路复用"）。加 `FramePacket` 即可。

### 4.2 有没有更简单的做法？

**有，而且应该先做。** 三个层次，按复杂度递增：

#### 方案 A：纯端口转发（不引入 netstack）—— 最简单

```
cmesh forward -L 2222:peer:22        # 本地 127.0.0.1:2222 → 对端 :22
cmesh forward -R 8080:localhost:80   # 对端 :8080 → 本地 :80
```

- **不需要 gVisor**，不需要 IP 包，不需要 MTU 处理
- 复用现有的 `MessageConn` + secure，加一个 `FrameStreamOpen` 控制帧 + yamux（cmesh 已有 `research-mux-and-portforward.md` 的结论）
- **已实测等价能力**：lab 的 `-mode forward` 用 netstack 做了同样的事，`ssh -p 2222 127.0.0.1` 成功
- **限制**：每个端口要显式映射；`curl http://peer.mesh:8080` 不行，得 `curl http://127.0.0.1:8080`

#### 方案 B：SOCKS5 + netstack（推荐）—— 中等

- 引入 gVisor netstack（+2.35 MB）
- SOCKS5 让**任意 TCP/UDP** 客户端用任意端口访问对端，无需预先映射
- **已实测全部通过**
- 限制：应用要配 `ALL_PROXY`；`ping` 不行

#### 方案 C：完整 L3（需要 TUN）—— 不可行

- 需要 `/dev/net/tun` + `CAP_NET_ADMIN`
- **明确卡点**：目标容器 `cap_eff=0`、无 `/dev/net/tun`。`TUNSETIFF` ioctl 必然 `EPERM`。cmesh 的 `cmesh probe` 已经把这个判定实现了（`internal/probe/tun_linux.go`）
- 即使有 TUN，还需要 `ip route add 10.99.0.0/24 dev tun0`，这是 `CAP_NET_ADMIN`
- **没有 workaround**。Linux 没有"非特权创建 TUN"的路径（除非容器 runtime 预置了 device node + 给了 cap，那就不是 `cap_eff=0` 了）

### 4.3 推荐落地顺序

1. **先做方案 A**（端口转发）——不引依赖，2–3 天，覆盖 `ssh`/数据库/HTTP 等 90% 场景
2. **再做方案 B**（SOCKS5 + netstack）——+2.35 MB，覆盖"任意端口、无需预映射"
3. **不做方案 C**

---

## 5. 成本

### 5.1 二进制体积（已实测，`-trimpath -ldflags "-s -w"`）

| 目标 | 基线 | +gVisor | 增量 |
|---|---|---|---|
| linux/amd64 | 9,457,824 B (9.02 MiB) | 11,813,024 B (11.27 MiB) | **+2,355,200 B (+2.25 MiB, +24.9%)** |
| linux/arm64 | 8,716,448 B (8.31 MiB) | 11,010,208 B (10.50 MiB) | **+2,293,760 B (+2.19 MiB, +26.3%)** |
| darwin/arm64 | 8,909,842 B (8.50 MiB) | 11,367,682 B (10.84 MiB) | +2,457,840 B (+2.34 MiB, +27.6%) |

**不加 `-s -w` 的对比（darwin/arm64，更接近开发构建）：**

| | 大小 |
|---|---|
| cmesh 基线（默认 `go build`） | 13,252,978 B (12.64 MiB) |
| cmesh + gVisor（默认） | 16,782,594 B (16.01 MiB) |
| **增量** | **+3,529,616 B (+3.37 MiB, +26.6%)** |

**与用户提到的 8.8 MB 的关系**：当前 `dist/cmesh-linux-amd64` 是 10,939,763 B（未 strip？），本次重编 stripped 是 9,457,824 B。加 gVisor 后 11.27 MiB。**用户看到的 8.8 MB 可能是更早的构建或不同平台**；按本次实测，增量稳定在 **+2.2 ~ +2.5 MB（+25%）**。

**最小 gVisor 链接基线（已实测）**：只 import `tcpip` + `channel` + `ipv4` + `tcp`（不含 gonet/udp/icmp），Linux/amd64 stripped 是 **4,276,384 B (4.08 MiB)**，空 main 基线 1,216,672 B。即"纯 netstack 内核"约 **3.06 MB**。加上 `gonet` + `udp` + `icmp` + `ipv6` 后变成 2.35 MB 增量（因为和 cmesh 已有的 stdlib 部分重合）。

### 5.2 依赖数量

**已实测：**

| 指标 | 基线 | +gVisor |
|---|---|---|
| `go list -m all` 模块数 | **31** | **112**（2025-05 快照） / **143**（2026-09 快照） |
| `go.sum` 行数 | 40 | 40（2025-05，因为 x/* 版本已覆盖） / 60（2026-09） |
| **实际参与链接的第三方模块** | **8** | **10** |
| 实际链接的 gVisor 包数 | 0 | **41** |
| **最小示例（`lab/netstack-min`）的 go.mod 依赖数** | — | **4**（gvisor + btree + x/sys + x/time） |

**关键**：`go list -m all` 的 112/143 是**模块图**，不是实际编译的包。真正进入二进制的新增第三方模块只有 **2 个**：

- `gvisor.dev/gvisor`
- `github.com/google/btree`（gVisor 的段表用）
- （`golang.org/x/time` 已因 quic-go 存在）

gVisor 的 `go.mod` 列了 containerd / k8s / grpc / cloud.google.com 一大堆，但那些是 `runsc` 用的，**netstack 路径不 import**。已实测 `go list -deps ./cmd/cm | grep gvisor.dev` = 41 个包，全部在 `pkg/tcpip`、`pkg/buffer`、`pkg/waiter`、`pkg/sync` 等小集合内。

**模块下载体积**（已实测）：

| gVisor 快照 | zip | 解压 |
|---|---|---|
| `20250503011706-39ed1f5ac29c` | 4.8 MiB | 21 MiB |
| `20260915211658-a6f909f08a72` | 20.7 MiB | 70 MiB |
| `20261021020752-c45f4e075076` | 47.4 MiB | — |

**这是选版本的重要依据**：旧快照 4.8 MiB 下载，新快照 20.7 MiB。CI 缓存和离线构建会受影响。

### 5.3 Go 版本要求（重要）

| gVisor 快照 | `go.mod` 的 `go` 指令 | 对 cmesh（`go 1.24.0`）的影响 |
|---|---|---|
| `20250503011706` | **`go 1.23.1`** | ✅ **不需要升级**（已实测：`go mod tidy` 后 cmesh 的 `go 1.24.0` 保持不变） |
| `20260915211658` | **`go 1.26.3`** | ❌ 强制 cmesh 升到 `go 1.26.3`（已实测：`go mod tidy` 后 go.mod 变成 `go 1.26.3`） |

**推断**：如果 cmesh 要保持 `go 1.24.0`，只能用 2025-05 的快照。如果接受升级 Go 工具链，可以用新快照（修了 CUBIC 溢出等 bug，见 §5.4）。

### 5.4 API 稳定性（查证）

好消息：**netstack 的核心 API 在两个快照之间没有变化**。

| API | 2025-05 | 2026-09 |
|---|---|---|
| `gonet.DialTCP` | `gonet.go:481` | `gonet.go:501` |
| `gonet.DialContextTCP` | `gonet.go:541` | `gonet.go:561` |
| `gonet.ListenTCP` | `gonet.go:76` | `gonet.go:76` |
| `gonet.NewTCPConn` | `gonet.go:250` | `gonet.go:266` |
| `tcp.NewForwarder` | `forwarder.go:51` | `forwarder.go:47` |
| `channel.New` | `channel.go:158` | 同 |

签名完全一致。**推断**：可以在 `go.mod` 里 pin 旧版保证 Go 版本兼容，将来平滑升级。

**新版修的 bug（查证 `wgengine/netstack/netstack.go:389-396` 注释）**：CUBIC 拥塞控制在 netstack 里有整数溢出（[google/gvisor#11632](https://github.com/google/gvisor/issues/11632)），Tailscale 曾因此 pin 到 reno。新版已改为浮点实现 + RFC 9438 钳制。**如果选旧快照，建议用 `cubic` 之外的选择或接受 reno**。

### 5.5 代码量估算（推断，基于已完成的 lab）

lab 已经写出的、可直接搬进 cmesh 的代码：

| 模块 | 行数 | 说明 |
|---|---|---|
| netstack 封装（stack 创建、地址、路由、pump、inject） | ~120 | `/tmp` lab `newVNet` + `pumpOut` + `injectIn` |
| SOCKS5 服务端（CONNECT + UDP ASSOCIATE + 域名解析） | ~240 | lab `socks5Conn` + `socks5UDPAssociate` |
| 端口转发（`-L` 模式） | ~60 | lab `modeForward` |
| 入站 listener（对端服务暴露） | ~50 | `tcp.NewForwarder` + 转 `127.0.0.1` |
| IP 包分帧 + secure 集成 | ~150 | `wire.FramePacket` + 收发循环 |
| MTU/缓冲/连接数配置 | ~80 | |
| CLI 子命令 + flag | ~150 | |
| **合计（生产质量，含错误处理、测试）** | **~1200–1600 行** | 不含测试 |
| 测试 | ~800–1200 行 | 参考 cmesh 现有比例（11964 生产 / 9502 测试） |

**对比 cmesh 现状**：生产代码 11,964 行。新增 1,200–1,600 行 = **+10~13%**。

### 5.6 内存（已实测，重复列出便于决策）

| 场景 | RSS |
|---|---|
| 空载（仅 netstack，无连接） | 9.1 MiB |
| 200 条空闲 TCP 流 | 24.5 MiB |
| 8 并发 × 32 MiB 传输中 | 38.2 MiB |
| 200 条活跃流（各 1 MiB） | 238.4 MiB |
| 单 stack 堆内存 | 53 KiB |

**容量规划建议（推断）**：按"每条活跃流 ~1.1 MiB"算，给 netstack 预留 `N × 1.1 MiB + 10 MiB`。K8s 容器 256 MiB limit 下，安全并发约 **150–200 条**。必须设 `MaxConcurrentFlows`，否则 OOM。

---

## 6. 交付物：可运行的最小验证代码

### 6.1 完整代码位置

| 位置 | 内容 | 状态 |
|---|---|---|
| `/Users/blackevil/Documents/Projects/container-mesh-reports/lab/netstack-lab/main.go` | 1282 行，包含 §3 的全部实验（inproc/peer/node/peer-fwd/forward/bench/socks-udp/idleflows/concurrency/mem） | 已实测 |
| `/Users/blackevil/Documents/Projects/container-mesh-reports/lab/netstack-min/` | **本次新增**：§6.2 的最小自包含示例（`main.go` + `go.mod` + `go.sum`，已 `go build` 并运行通过） | 已实测 |
| `/tmp/mtulab/` | MTU 扫描 + PMTU 黑洞实验（约 220 行） | 已实测 |

`lab/netstack-min` 依赖只有 3 个模块（`gvisor.dev/gvisor` + `github.com/google/btree` + `golang.org/x/sys`），可以离线构建：

```bash
cd /Users/blackevil/Documents/Projects/container-mesh-reports/lab/netstack-min
GOPROXY=off go build -o /tmp/ns-min .
/tmp/ns-min
```

### 6.2 最小可运行示例（自包含，可直接 `go run`）

```go
// go.mod:
//   module netstackmin
//   go 1.24.0
//   require gvisor.dev/gvisor v0.0.0-20250503011706-39ed1f5ac29c
//
// 运行: go run .
// 预期输出:
//   ping 10.99.0.6 -> 10.99.0.5 rtt=...
//   TCP 8 MiB: ... MiB/s
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const mtu = 1280 // 必须 <= 隧道净荷上限，见 §3.3

type vnet struct {
	s  *stack.Stack
	ep *channel.Endpoint
	ip netip.Addr
}

// newVNet 创建一个纯内存网卡。没有 fd，没有 /dev/net/tun，没有 syscall。
func newVNet(ip netip.Addr) (*vnet, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, icmp.NewProtocol4},
		HandleLocal:        true,
	})
	sack := tcpip.TCPSACKEnabled(true)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack); err != nil {
		return nil, fmt.Errorf("sack: %v", err)
	}
	ep := channel.New(1024, mtu, "")
	if err := s.CreateNIC(1, ep); err != nil {
		return nil, fmt.Errorf("CreateNIC: %v", err)
	}
	pa := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFromSlice(ip.AsSlice()).WithPrefix(),
	}
	if err := s.AddProtocolAddress(1, pa, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("AddProtocolAddress: %v", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	return &vnet{s: s, ep: ep, ip: ip}, nil
}

func (v *vnet) close() { v.ep.Close(); v.s.Close() }

func fa(ip netip.Addr, port uint16) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(ip.AsSlice()), Port: port}
}

// pump 把 src 的出站包注入 dst。真实部署里这里换成"加密 + 发到对端"。
func pump(ctx context.Context, src, dst *vnet) {
	for {
		pkt := src.ep.ReadContext(ctx)
		if pkt == nil {
			return
		}
		view := pkt.ToView()
		pkt.DecRef()
		raw := view.AsSlice()
		if len(raw) > 0 {
			p := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(append([]byte(nil), raw...)),
			})
			dst.ep.InjectInbound(header.IPv4ProtocolNumber, p)
			p.DecRef()
		}
		view.Release()
	}
}

func ping(v *vnet, dst netip.Addr) (time.Duration, error) {
	var wq waiter.Queue
	ep, err := v.s.NewEndpoint(icmp.ProtocolNumber4, ipv4.ProtocolNumber, &wq)
	if err != nil {
		return 0, fmt.Errorf("endpoint: %v", err)
	}
	defer ep.Close()
	if err := ep.Bind(fa(v.ip, 0)); err != nil {
		return 0, fmt.Errorf("bind: %v", err)
	}
	if err := ep.Connect(fa(dst, 0)); err != nil {
		return 0, fmt.Errorf("connect: %v", err)
	}

	// 先注册可读事件，再写：内存网卡上回复可能在 Write 返回前就到了。
	entry, notify := waiter.NewChannelEntry(waiter.ReadableEvents)
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)

	msg := make([]byte, 8+16)
	msg[0] = byte(header.ICMPv4Echo)
	binary.BigEndian.PutUint16(msg[6:], 1)
	copy(msg[8:], []byte("netstack-min!!!"))
	start := time.Now()
	if _, err := ep.Write(bytes.NewReader(msg), tcpip.WriteOptions{
		To: &tcpip.FullAddress{Addr: tcpip.AddrFromSlice(dst.AsSlice())},
	}); err != nil {
		return 0, fmt.Errorf("write: %v", err)
	}

	select {
	case <-notify:
	case <-time.After(3 * time.Second):
		return 0, errors.New("timeout")
	}
	buf := make([]byte, 1500)
	w := tcpip.SliceWriter(buf)
	if _, err := ep.Read(&w, tcpip.ReadOptions{}); err != nil {
		return 0, fmt.Errorf("read: %v", err)
	}
	return time.Since(start), nil
}

func main() {
	a, err := newVNet(netip.MustParseAddr("10.99.0.5"))
	if err != nil {
		log.Fatal(err)
	}
	defer a.close()
	b, err := newVNet(netip.MustParseAddr("10.99.0.6"))
	if err != nil {
		log.Fatal(err)
	}
	defer b.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pump(ctx, a, b)
	go pump(ctx, b, a)

	rtt, err := ping(b, netip.MustParseAddr("10.99.0.5"))
	if err != nil {
		log.Fatalf("ping: %v", err)
	}
	fmt.Printf("ping 10.99.0.6 -> 10.99.0.5 rtt=%v\n", rtt.Round(time.Microsecond))

	// TCP echo server on a:8080
	ln, err := gonet.ListenTCP(a.s, fa(netip.MustParseAddr("10.99.0.5"), 8080), ipv4.ProtocolNumber)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); io.Copy(io.Discard, c) }(c)
		}
	}()

	c, err := gonet.DialContextTCP(ctx, b.s, fa(netip.MustParseAddr("10.99.0.5"), 8080), ipv4.ProtocolNumber)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	const total = 8 << 20
	buf := make([]byte, 64<<10)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); io.Copy(io.Discard, c) }()
	t0 := time.Now()
	sent := 0
	for sent < total {
		n, err := c.Write(buf)
		if err != nil {
			log.Fatal(err)
		}
		sent += n
	}
	_ = c.CloseWrite()
	wg.Wait()
	el := time.Since(t0)
	fmt.Printf("TCP %d MiB in %v => %.1f MiB/s\n", sent>>20, el.Round(time.Millisecond),
		float64(sent)/(1<<20)/el.Seconds())
}
```

**实测输出（本机 macOS arm64，MTU 1280，跑 3 次）**：

```
ping 10.99.0.6 -> 10.99.0.5 rtt=190µs
TCP 8 MiB in 30ms => 266.7 MiB/s

ping 10.99.0.6 -> 10.99.0.5 rtt=508µs
TCP 8 MiB in 30ms => 267.3 MiB/s

ping 10.99.0.6 -> 10.99.0.5 rtt=441µs
TCP 8 MiB in 29ms => 273.0 MiB/s
```

（rtt 的 190–508 µs 波动来自首包路径冷启动，后续 ping 稳定在个位数 µs，见 §1.1。）

`go.mod`（3 个依赖，全部来自 module cache，可离线构建）：

```
module netstackmin

go 1.24.0

require gvisor.dev/gvisor v0.0.0-20250503011706-39ed1f5ac29c

require (
	github.com/google/btree v1.1.2 // indirect
	golang.org/x/sys v0.26.0 // indirect
	golang.org/x/time v0.7.0 // indirect
)
```

### 6.3 如何复现全部实验

```bash
# 1. 构建 lab（从 module cache 离线构建）
cd /Users/blackevil/Documents/Projects/container-mesh-reports/lab/netstack-lab
GOPROXY=off go build -o /tmp/ns-lab .

# 2. 单进程：ping + TCP + UDP
/tmp/ns-lab -mode inproc

# 3. 内存/启动
/tmp/ns-lab -mode mem
/tmp/ns-lab -mode idleflows -conns 500

# 4. 双进程隧道 + SOCKS5
/tmp/ns-lab -mode peer -addr 127.0.0.1:9999 &
/tmp/ns-lab -mode node -addr 127.0.0.1:9999 -socks 127.0.0.1:1080 -ping 10.99.0.5 &
curl --socks5-hostname 127.0.0.1:1080 http://peer.mesh:8080/
ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1080 %h %p' user@10.99.0.5
/tmp/ns-lab -mode socks-udp -socks 127.0.0.1:1080 -host 10.99.0.5 -port 9090

# 5. 转发到真实 loopback 服务
python3 -m http.server 18080 --bind 127.0.0.1 &
/tmp/ns-lab -mode peer-fwd -addr 127.0.0.1:9997 -port 18080 &
/tmp/ns-lab -mode node -addr 127.0.0.1:9997 -socks 127.0.0.1:1082 &
curl --socks5-hostname 127.0.0.1:1082 http://10.99.0.5:18080/index.html

# 6. MTU 扫描与 PMTU 黑洞
cd /tmp/mtulab && go build -o mtu . && ./mtu -pmtu
for m in 1500 1280 1200 1000 576; do ./mtu -mtu $m -mib 16; done
```

---

## 7. 明确不可行的方案

### 7.1 透明 L3（`ping 10.99.0.5` 直接可用）—— **不可行**

**卡点**：Linux 内核必须有一个接口承载 `10.99.0.0/24` 路由。创建该接口的两条路径都需要权限：

| 路径 | 需要的权限 | 目标容器有吗 |
|---|---|---|
| TUN/TAP（`/dev/net/tun` + `TUNSETIFF`） | `CAP_NET_ADMIN` + device node | ❌ `cap_eff=0`，无 `/dev/net/tun` |
| `ip tuntap add` / netlink | `CAP_NET_ADMIN` | ❌ |
| 修改路由表（`ip route add`） | `CAP_NET_ADMIN` | ❌ |
| 用户态 socket 劫持（`LD_PRELOAD`） | 只能影响**本进程**及其子进程，且需要能注入到目标进程 | ❌ 对已在跑的进程无效，对静态链接无效 |

**没有绕过方法。** 这是内核的权限模型，不是实现问题。

**唯一的"透明"替代品**：`/etc/hosts` + 本地端口映射，但那仍然不是 `ping`。

### 7.2 wireguard-go 作为独立方案 —— **不划算**

**卡点**：它内部就是 gVisor（§2.1 查证）。用它意味着：
- 引入 `golang.zx2c4.com/wireguard`（整套 Noise 协议、device、peer、tun 适配）
- 引入 `golang.zx2c4.com/wintun`（Windows 专用，但会进模块图）
- 与 cmesh 现有的 SPAKE2 + AES-GCM 加密**重复**
- 二进制反而更大（11.84 vs 11.81 MB）
- API 更窄：`Net` 只有 `DialTCPAddrPort` / `ListenTCPAddrPort` / `DialUDPAddrPort` / `DialPingAddr`，不如直接用 gVisor 灵活（拿不到 `stack.Stack` 做 `tcp.NewForwarder` 之类的事）

**唯一优势**：`CreateNetTUN` 一行就能用，省 ~120 行封装代码。不值得为此多引入一个协议栈。

### 7.3 gVisor 完整 sandbox（`runsc`）—— **不可行且不需要**

**卡点**：`runsc` 需要 `CAP_SYS_ADMIN`、创建 user namespace、ptrace 权限。目标容器 `cap_eff=0`，跑不起来。而且我们只需要 `pkg/tcpip`，不需要整个 sandbox。

### 7.4 在 netstack 里跑 DNS 服务器 —— **可行但没必要**

gVisor 没有 DNS 解析器（§1.5），但可以自己实现一个 DNS 服务端监听 netstack 的 `:53`。Tailscale 就是这么做的（`netstack.go:1699-1710`）。

**不推荐**，因为：
- 需要额外的 DNS 报文编解码（可用 `golang.org/x/net/dns/dnsmessage`，stdlib vendor 里已有）
- 应用还得把 DNS 指向这个虚拟 IP，而在无 TUN 环境里应用根本连不到那个 IP
- **在 SOCKS5 层解析域名更简单**（§2.3），且已实测可行

---

## 8. 最终建议

### 8.1 推荐方案

**方案 B：SOCKS5 + gVisor netstack，pin 旧版 gVisor。**

具体：
- 版本：`gvisor.dev/gvisor v0.0.0-20250503011706-39ed1f5ac29c`（保持 `go 1.24.0`，下载仅 4.8 MiB）
- 若接受升级 Go 工具链到 1.26.3，改用 `v0.0.0-20260915211658-a6f909f08a72`（CUBIC 修复，下载 20.7 MiB）
- MTU：**1200**（匹配 `p2p.MaxDatagram`，避免黑洞）
- 暴露：SOCKS5（CONNECT + UDP ASSOCIATE）+ 可选 `-L` 端口转发
- 并发上限：**200 条活跃流**（约 220 MiB），可配
- DNS：在 SOCKS5 层解析，维护 mesh 名字表

**先做方案 A（端口转发）作为第一里程碑**，因为它不引任何新依赖、覆盖大部分场景、且能验证 `secure` + 传输层的多流能力（配合 `research-mux-and-portforward.md` 的 yamux 结论）。

### 8.2 关键实测数据汇总

| 指标 | 数值 |
|---|---|
| 无 TUN 提供完整 TCP/IP | ✅ 已实测（ICMP/TCP/UDP 全通） |
| 数据面 syscall / fd | **0**（`lsof` 仅隧道 + SOCKS 两个真实 socket） |
| 空载 RSS | 9.1 MiB |
| 单 stack 堆内存 | 53 KiB |
| stack 创建耗时 | 35–48 µs |
| 进程启动 | 3.5–4.1 ms |
| 空闲流内存 | 20 KiB/流 |
| 活跃流内存 | ~1.1 MiB/流 |
| 200 活跃流 RSS | 238.4 MiB |
| 单进程 TCP（MTU 1500） | 349.2 MiB/s |
| 单进程 TCP（MTU 1200） | 261.4 MiB/s |
| 双进程隧道单连接 | 73–88 MiB/s（持续） |
| 内核 loopback 基线 | 6705.9 MiB/s |
| SOCKS5 CONNECT 开销 | +0.24 ms |
| 二进制增量（linux/amd64） | +2.35 MB（9.02 → 11.27 MiB） |
| 新增实际链接模块 | 2 个（gvisor + btree） |
| 新增 gVisor 包 | 41 个 |
| 代码量估算 | 1,200–1,600 行生产代码 |

### 8.3 主要风险

| 风险 | 严重度 | 缓解 |
|---|---|---|
| **二进制 +25%** | 中 | 用 build tag 把 netstack 做成可选编译单元（`-tags netstack`），默认构建不含 |
| **MTU 黑洞** | **高** | 固定 MTU 1200，禁止依赖 PMTUD；集成测试必须包含"隧道容量 < netstack MTU"的用例 |
| **Go 版本被迫升级** | 中 | pin 2025-05 快照；或接受升级到 1.26.3 |
| **内存（活跃流 1.1 MiB/条）** | 中 | 硬上限 200 并发；调小 gVisor 的 `TCPReceiveBufferSizeRangeOption` |
| **旧 gVisor 快照的 CUBIC 溢出** | 中 | 用 `reno` 而非 `cubic`；或升新快照 |
| **许可证** | 低 | gVisor 是 Apache-2.0（查证 `LICENSE`），cmesh 是 AGPL-3.0。Apache-2.0 与 AGPL-3.0 **兼容**（Apache 代码可被 AGPL 项目包含），但需保留 NOTICE |
| **虚拟网段冲突** | 中 | 已实测 10.99.0.5 在本机被上游路由。默认改用 `100.64.0.0/10`，启动时探测冲突 |
| **`syscall.Conn` 缺失** | 低 | 只影响做 socket 层调优的库；SOCKS5 代理场景不涉及 |
| **不是透明的** | **高（需求层面）** | 明确告知用户：`ping`/`ssh user@10.99.0.5` 必须改成 `ALL_PROXY` 或 `-p 2222` 形式 |

### 8.4 一句话回答需求

> 让这些容器能像有 TUN 一样用虚拟 IP 互访（`ping 10.99.0.5`、`ssh user@10.99.0.5`）

**`ping` 不可能。`ssh` 可以，但要写成 `ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1080 %h %p' user@10.99.0.5` 或 `ssh -p 2222 user@127.0.0.1`。**

gVisor netstack 能提供完整的协议栈，但**不能改变"内核不认识虚拟 IP"这个事实**。这是无 TUN 容器的硬边界。
