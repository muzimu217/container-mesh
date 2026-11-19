# 设计稿：单进程多宿主（一台机器一个 cmesh 进程同时挂多个群组身份）

> 三层文档的第三层（具体怎么做）。上层：[`../REQUIREMENTS.md`](../REQUIREMENTS.md)（要什么）、
> [`../DESIGN.md`](../DESIGN.md)（用什么结构）。本文回答：**一个进程如何同时持有 N 个群组的
> 设备身份、N 条 discovery 会话和一条共享的直连监听 socket，而不破坏现有的认证与撤销模型。**
>
> **状态**：设计稿，未实现。需求编号待立（落地前按 T-0.1 在 REQUIREMENTS.md 编号）。
> 文中每条「当前代码是这样」的断言都标了 file:line（撰写时逐条核实，HEAD = `78add1a`）。
> 数字标注遵循 T-5.3：`[实测]` / `[推断]`。

**版本**：D1（2026-11-19）

> ✅ **§4 的 SNI 机制已实测通过**（2026-11-19，进程内真实 QUIC 握手，18 个实验全过，
> `internal/mesh/sni_select_test.go`）：
>
> - **QUIC 下 `GetCertificate` 会被调用，且 `chi.ServerName` 就是客户端填的 DeviceID**
>   （线级证明：解析原始 ClientHello 得到 `SNI host_name = "inycnepctiqwr6te"`）。
> - **DeviceID 可以直接当 SNI 用**：Go 的 crypto/tls **从不校验 SNI 的 DNS 合法性** ——
>   客户端只剥 IP 字面量，服务端原样塞进 `chi.ServerName`。实测矩阵全部握手成功：
>   下划线、空格、斜杠都照过；只有 IP 字面量会被剥掉（此时回落默认证书）。
> - **pin 未被削弱**：SNI 指向 A 却拿到 B 的证书 → 握手失败（`pin mismatch: peer CN=...`）。
>   `VerifyConnection` 读的是实际出示的证书，与 SNI 无关。
> - **旧客户端兼容有一个必须知道的实现细节**：Go 只在「ServerName 非空**或** Certificates
>   为空」时才调 `GetCertificate`。多身份监听器若把默认证书放进 `Certificates`，旧客户端
>   **根本不会进回调**。**做法：`Certificates` 留空，回调里对未命中 SNI 显式返回默认证书**
>   （不要返回 `nil, nil`）。
> - ⚠️ **必须加固的一处**：**TLS 会话恢复会完全绕过 `GetCertificate`**（实测第三次拨号
>   只收到 2 次回调）—— 恢复连接沿用首次会话的证书，身份路由被跳过。当前**不可利用**
>   （`deviceTLS` 不设 `ClientSessionCache`，恢复不会发生），但多宿主形态应显式
>   `SessionTicketsDisabled = true`：恢复对本产品零收益（每条直连是长连接），而它会静默
>   破坏身份路由。
> - 备选（SNI = `cm-<deviceid>.mesh`）与 ALPN 载体**也都实测可行**，但主方案用原文即可
>   —— DeviceID 本就是公开的成员视图字段（证书 CN 里就是它），无新增泄漏。
> - 仍未证实：Linux/容器环境的复跑、0-RTT 与恢复的交互、`GetConfigForClient` 拿不到 SNI
>   （那一层做不了路由）、跨进程/NAT 下 SNI 是否被改写。
>
> **文档其余部分（身份/连接/state 分层、规模估算、风险）不受影响。**

---

## 0. 摘要

1. **三层分开：身份按目录、连接按参数、监听按证书。** 设备身份今天就是「一个 state 目录
   一张 device.json」（`internal/cli/join_cred.go:144`），多宿主不改这一点——只是目录变成
   `<root>/groups/<名>/`。出站连接的 TLS 身份**天然按连接选**：每次拨号自带 Device
   （`internal/mesh/direct.go:971`、`internal/mesh/direct.go:1522`）。唯一要动的是**入站**：
   监听器今天一张证书服务所有连接（`internal/mesh/direct.go:430-444`），多宿主要把它改成
   「每握手按对端选身份」。

2. **每群组一个 discovery 客户端，一个共享监听器。** `discovery.Client` 已经是按实例持有
   身份与地址的（`internal/discovery/client.go:63-134`），N 个实例互不干扰；直连监听器
   共用一个 UDP 端口，入站按对端证书的 CN（DeviceID）路由到对应群组的成员视图做 pin。

3. **「one machine one announcer」不被违反。** 那条决策（issue #8 D-7，记录于
   `internal/discovery/client.go:79-83` 与 `internal/cli/meshcmd.go:86-91` 注释）防的是
   **同机多进程、同一 DeviceID** 的第二个 announcer 用空端点覆盖真 announcer 的宣告。
   多宿主是同一进程内 N 个**不同 DeviceID** 各自宣告，且宣告的端点集合本来就是同一台
   机器的同一组地址——冲突不存在。

4. **兼容：旧布局 = 单群组，不迁移也能跑。** `<root>/device.json` 直接存在就按单群组处理；
   `groups/` 目录出现后新群组才落进去。

5. **规模**：阶段一约 500–600 行生产 + 600–700 行测试（§9）；内存估算式见 §7。

---

## 1. 背景与需求

网主原话（D77 记录，`docs/decisions.md:1344` 起）：

> 「我可以是你们团体的，同时我也可以是他们团体的一员，就是我可以携带多个密钥身份进行入网……
> 只要我拿账号密码就可以进入对应的群组。」

**今天的形态**（`docs/decisions.md:1362`，D77 的边界一节如实记着）：

> 多群组单机：今天靠「每群组一个 `--state` 目录、各跑一个 join 进程」实现；单进程多身份
> （多宿主）是后续需求，未开工。

可行但有三个缺点：

| 缺点 | 量级 | 依据 |
|---|---|---|
| N 份进程内存 | 每进程空闲 RSS 6.3–9.0 MB `[实测]`（`docs/ROUND-LOG.md:535`，macOS 空闲基线；join 常驻含 bench 时 15.6–15.8 MB `[实测]`，`docs/ROUND-LOG.md:520`） | N=5 → 约 40 MB 起步 |
| N 条 discovery 连接 | 每客户端一条 members 流 + 30 s 一次 register（`internal/discovery/client.go:718`、`internal/discovery/member.go:36`） | 本不可避免，但可共享 socket 层以下的机器端点 |
| N 个 systemd unit 的运维面 | 每群组一个 unit、一份日志、一份重启策略 | 运维诉求，无代码基线 |

**目标**：`cmesh join` 一次进程挂 N 个群组；`cmesh status` 一眼看清所有群组；踢一个群组
不影响其他群组；旧的「一目录一进程」继续可用。

---

## 2. 当前代码事实（逐条核实）

写设计前先把「现在是这样」钉死。以下断言全部对应 HEAD `78add1a` 的真实代码。

### 2.1 身份层

| 事实 | 位置 |
|---|---|
| DeviceID = SHA-256(公钥) 前 80 bit，base32 16 字符，自证公钥 | `internal/identity/identity.go:78-81` |
| 设备 TLS 证书 = 自签，Subject CN = DeviceID，公钥即身份密钥 | `internal/identity/identity.go:137-159` |
| device.json = Argon2id（64 MiB）派生密钥 + AES-256-GCM 加密的私钥文件，0600 | `internal/identity/identity.go:212-246`（Save）、`:252-286`（LoadDevice） |
| 默认 state 目录 = `~/.cmesh`；`--state` 显式覆盖 | `internal/cli/mesh.go:22-31`、`internal/identity/identity.go:204` |
| `join --cred` 会在 state 目录里自助创建设备身份（一目录一身份的来源） | `internal/cli/join_cred.go:144-158`（ensureDeviceIdentity） |
| state 目录里 join 落盘：`device.json` + `discovery.json`（`discoveryConfig`：account、discovery_addrs、endpoints）+ `inbox/` | `internal/cli/discovery.go:361-371`（saveDiscoveryConfig）、`:241-248`（结构体）、`:533`（inboxDir） |

### 2.2 discovery 客户端（每实例绑定一个身份）

| 事实 | 位置 |
|---|---|
| `ClientConfig.Device` 是客户端签发 announce 与流凭据的唯一身份 | `internal/discovery/client.go:66-68` |
| `Run(ctx)` = heartbeat 循环 + streamLoop，各客户端独立 | `internal/discovery/client.go:517-595` |
| 每客户端一条 members 流（streamLoop 持一条 WS 连接，断线退避重连） | `internal/discovery/client.go:718-782` |
| Announce 携带 `{DeviceID, Endpoints, Timestamp}` 并以设备签名 | `internal/discovery/client.go:318-341` |
| HeartbeatInterval = 30 s | `internal/discovery/member.go:36` |
| one-shot 命令（mesh ping/dial/msg…）不宣告（NoAnnounce），理由见 §5.3 | `internal/cli/meshcmd.go:96-105`、`internal/discovery/client.go:79-83` |

### 2.3 直连与中转的 TLS 身份来源

**出站（拨号侧）——按连接选身份已经天然成立**：

| 事实 | 位置 |
|---|---|
| `DialConfig.Device` 是每次拨号的入参；`DialDirect` 内部当场用该身份建证书和 tls.Config | `internal/mesh/direct.go:916-924`（字段）、`:971-984`（cert+tlsCfg） |
| `dialOne` 每次端点尝试开自己的 UDP socket（quic-go 单用途 Transport），socket 与 tlsCfg 均不跨尝试共享 | `internal/mesh/direct.go:1045-1068`、`:1070-1071` |
| `DialRelaySession(dev, …)` 同样每次调用拿身份建证书与 tls.Config，ALPN 固定 relay | `internal/mesh/direct.go:1505-1539`（`:1522` cert、`:1539` NextProtos） |
| 中转干道的加入端 `JoinRelayTrunk` → `DialRelaySession(cfg.Device, …)`，身份来自 `RelayJoinConfig.Device` | `internal/mesh/relaytrunk.go:2653-2655`、`:2712` |
| 结论：**只要每个群组各发各的 `DialConfig.Device`，N 个身份的出站连接零改动即正确** | — |

**入站（监听侧）——今天不是按连接的，是本设计唯一的核心改造点**：

| 事实 | 位置 |
|---|---|
| `ListenerConfig.Device` 是单一身份；`NewListener` 建一次证书（`:430`）、一个 tls.Config（`:444`），交给 `quic.Listen`（`:479`）——**所有入站连接共用同一张证书** | `internal/mesh/direct.go:370-371`、`:430`、`:444`、`:479` |
| listener 的 pin 函数绑定单一 `cfg.Members` resolver：对端证书 CN 的公钥只对这一个视图核验 | `internal/mesh/direct.go:434-443` |
| 对端身份从对端证书的 CN 读出：`peerID := DeviceID(PeerCertificates[0].Subject.CommonName)` | `internal/mesh/direct.go:578` |
| ALPN 已按连接协商分流（D76 先例）：`NextProtos = [directALPN, RelayALPN]`（有 trunk 时），`handle` 按 `NegotiatedProtocol` 分发 | `internal/mesh/direct.go:460-463`、`:563-574` |
| inbox 是 ListenerConfig 的单一目录；msg/file 回调不携带群组概念 | `internal/mesh/direct.go:380-385`、`internal/cli/discovery.go:533` |

### 2.4 join 命令的编排（多宿主要循环化的部分）

`runJoin`（`internal/cli/discovery.go:405-897`）今天的编排顺序：

1. 解析 `--state`（`:408`）、`--cred`（`:424`）、`--listen`（`:413`）、`--relay-trunk`（`:425`）；
2. `--cred` 路径：解码凭证 → ensureDeviceIdentity（一目录一身份）→ enroll（`:465-490`）；
3. **先绑监听器**（`--listen` 时）：`mesh.NewListener(Device: dev, Members: resolver, InboxDir: <dir>/inbox)`（`:534-576`）——监听先于 client，绑定结果决定宣告端点；
4. `discovery.NewClient(Addrs: inv.DiscoveryAddrs, Device: dev, Endpoints: announced)`（`:578-587`）；
5. `resolver.set(cl)` **无条件**（纯出站成员的视图也曾因此永远为空，round-61 缺陷 B 的修复，`:591-603`）；
6. 保存 `discovery.json`（`:635-645`）；
7. 凭证成员起 `certRenewLoop`（`:734-736`）；`--relay-trunk` 起 `joinRelayTrunk` goroutine（`:745-752`，重试在调用侧，`internal/mesh/relaytrunk.go:2685-2690`）；
8. 视图打印循环 + SIGINT/SIGTERM graceful leave（`:758-896`）。

**这个编排是「单群组」的**：一个 dev、一个 client、一个 resolver、一个监听器。多宿主 =
把这个编排变成 per-group 的循环，共享第 3 步的监听器。

---

## 3. 身份拓扑：一个进程 N 个身份，按连接选证书

### 3.1 模型

```
进程 cmesh join --state <root>
├── group A: identity.Device(AD) ── discovery.Client(A) ── anchor A（members 流 + heartbeat）
│              └─ 出站拨号用 AD 的证书（per-dial tls.Config）
├── group B: identity.Device(BD) ── discovery.Client(B) ── anchor B
└── 共享 Listener（一个 UDP 端口）
     └─ 入站：按对端证书 CN=peerID，在 {A,B} 的视图中找含 peerID 的那个视图做 pin
```

- **N 个身份 = N 个独立密钥对**。同一台机器在两个群组各有一张 device.json，两个 DeviceID
  不同（不同公钥 → 不同哈希，`internal/identity/identity.go:78`）。口令可以相同（Argon2id
  每文件独立盐，`internal/identity/identity.go:220-228`），也可以不同；`--device-passphrase`
  在多群组下作用于「本次操作涉及的所有身份」，交互式时逐个询问（阶段二 polish，§9）。
- 撤销天然按群组隔离：`cred revoke` 级联删的是**该凭证的委派签的成员**（D77，
  `docs/decisions.md:1350,1355`；`internal/identity/delegation.go` 的 RevokeDelegation），
  另一群组的身份由另一个账号/凭证签发，互不触碰。

### 3.2 出站：零改动（证据链）

出站选身份在现有代码里**天然成立**，理由是三条已核实的代码事实：

1. `DialDirect` 的证书来自 `cfg.Device.TLSCertificate()`（`internal/mesh/direct.go:971`），
   而 `cfg.Device` 是**每次调用**的入参（`:916-924`）——不存在进程级单例身份。
2. 每次端点尝试有自己的 UDP socket 与 tls.Config（`internal/mesh/direct.go:1070-1071`），
   拨号之间不共享任何 TLS 状态。
3. 中转路径同理：`DialRelaySession` 每次调用拿 `dev` 建证书（`internal/mesh/direct.go:1522`），
   trunk join 从 `RelayJoinConfig.Device` 传入（`internal/mesh/relaytrunk.go:2712`）。

所以多宿主的出站侧只需要**编排**：群组 A 的 `mesh ping` 用 A 的 Device 与 A 的视图
resolver；群组 B 同理。`internal/mesh` 包一行不改。

### 3.3 入站：监听器的每握手选身份（本设计的核心新增）

**现状**：一张证书 + 一个 tls.Config 服务所有入站连接（`internal/mesh/direct.go:430-444,479`）。
**目标**：一个 UDP 端口，入站连接按「对端属于哪个群组」选择我们这边出示的证书与 pin 视图。

QUIC/tls 库层已确认可行——**D76 的同端口双 ALPN 是同一机制的在产先例**：ALPN/证书协商都是
每握手进行的，一个监听器可以把多个候选放进 tls.Config，由每条连接的握手结果分流
（裁定与核实过程：`docs/decisions.md:1310-1337`，真机实测：`docs/ROUND-LOG.md:1429`，
代码落地：`internal/mesh/direct.go:460-463,563-574`）。证书维度同理：Go 的
`tls.Config.GetCertificate` 回调**每握手调用一次**，quic-go 走标准 crypto/tls，服务端
可在回调里按 ClientHello 选证书。

**难题**：ClientHello 里可用来路由的信息只有 SNI，而今天的拨号端不设 ServerName
（`deviceTLS` 不填，对端是裸 IP，`internal/mesh/direct.go:342-346`）。

**方案（SNI = 目标 DeviceID，向后兼容）**：

1. 拨号端：`deviceTLS` 在多宿主形态下把 `ServerName` 设为**拨号方期望的目标设备 ID**。
   这个 ID 它本来就握着——pin 函数核验的就是它（`internal/mesh/direct.go:975-983`），
   不新增任何知识。
2. 监听端：`tls.Config.GetCertificate(chi)` 按 `chi.ServerName` 查
   `deviceID → 群组身份` 表：
   - 命中 → 出示该群组身份的证书；
   - 未命中（旧拨号端不带 SNI，或 SNI 是本进程没有的 ID）→ **回落出示默认身份**（第一个
     加入的群组）。旧拨号端随后按自己的视图 pin 我们的证书：如果默认身份恰是它视图里的
     那个（单宿主机器的常态），握手成功；不是则 pin 失败、拨号报 `ErrAuth`——与今天
     「拨错机器」的错误同形，不引入新失败类别。
3. pin 侧：listener 的 `VerifyConnection` 从单一 resolver 改为**多 resolver 依次尝试**：
   按对端证书 CN（`internal/mesh/direct.go:360` 已把 CN 解析成 DeviceID）在 N 个群组的
   视图里找含该 ID 的视图，用该视图的 key 核验公钥。全都不含 → 拒绝（与今天「不是成员」
   同形）。DeviceID 是公钥哈希、全局自证（`internal/identity/identity.go:78`），同一
   CN 在多个群组视图同时出现只有一种情形：对端同一物理设备的同一身份被两个群组各自
   批准——此时两个视图记录的是同一把公钥，用哪个视图 pin 结果相同（§8.4 分析边界）。
4. 会话归属：pin 成功即确定群组（含对端 ID 的那个视图）。serve 层把群组名传给
   msg/file/exec 回调与 InboxDir 解析（§8.1）。

**协议兼容性**：SNI 是加法——旧服务端只有一张证书、没有 GetCertificate 路由，收到的
SNI 被忽略（crypto/tls 无 SNI 匹配要求时照常握手），单宿主机器行为不变；新拨号端对旧
服务端发 SNI 也无害。多宿主拨号端对多宿主服务端：SNI 命中 → 正确证书 → pin 过。

**为什么不另开 N 个端口**：每群组一个监听器会回到「N 个端口 N 条防火墙规则」的运维面，
且宣告端点也要按群组分裂（§8.3）；共享端口 + 每握手选证书是 D76 已验证的同构做法。

---

## 4. state 布局

```
<root>/                          # --state 指向的目录
├── device.json                  # 旧布局（单群组）——原样保留，不迁移
├── discovery.json               # 旧布局
├── inbox/                       # 旧布局
└── groups/                      # 新布局：每群组一个子目录
    ├── <群组名>/
    │   ├── device.json          # 该群组的设备身份（加密于口令，identity.go:212）
    │   ├── discovery.json       # 该群组的账号/锚点/已宣告端点（discovery.go:241-248）
    │   └── inbox/               # 该群组的入站文件（§8.1）
    └── <群组名2>/…
```

**兼容规则**：

| `<root>` 的形态 | 行为 |
|---|---|
| 有 `device.json`、无 `groups/` | **单群组模式**，群组名固定 `default`。所有旧命令语义不变 |
| 有 `groups/` | 多宿主模式；`groups/default` 可以存在（旧布局显式迁移或新机器直接用） |
| 两者并存 | `default` 优先读旧布局文件；给出迁移提示但不自动移动文件（不迁移也能跑） |

规则的理由：`ensureDeviceIdentity` 与 `openMeshSession` 都按 `<dir>/device.json` 定位
（`internal/cli/join_cred.go:145`、`internal/cli/meshcmd.go:55`），把「目录」抽象成
「resolveGroupDir(root, group)」一个函数即可同时服务两种布局；群组名做文件名安全性校验
（`[a-z0-9-]{1,32}`，拒绝 `..`、`/`）。

**discovery.json 不加字段**：它已经是「账号 + 锚点 + 端点」的完备记录
（`internal/cli/discovery.go:241-248`），每群组一份即可；群组名由目录位置表达，不落盘。

---

## 5. 连接拓扑

### 5.1 每群组：一条 discovery 客户端

`discovery.Client` 是为多实例设计的：身份、锚点、端点、日志全部在 `ClientConfig`
（`internal/discovery/client.go:63-134`），实例间零共享状态。多宿主 = N 个 `Client`
各自 `Run(ctx)`：

- N 条 members 流、每客户端每 30 s 一次 register（`internal/discovery/member.go:36`）——
  连接数与今天「N 进程」完全相同，没有新增负载；
- N 个身份对各自锚点宣告各自的端点集合；
- 每群组独立 graceful leave（`internal/discovery/client.go:436-462`）：SIGINT 时按群组
  依次广播 LEAVE（各自 bounded 2 s，`internal/cli/discovery.go:399`）。

### 5.2 共享：一个直连监听器

`--listen` 在多宿主下只解析一次：一个 UDP socket（`net.ListenUDP`，`internal/mesh/direct.go:474`）、
一个 `quic.Listener`，服务所有群组的入站直连。证书按 §3.3 的每握手路由。

入站连接的身份判定路径（改造后）：

```
ClientHello ──SNI(可选)──▶ GetCertificate 选我方证书
握手完成 ──对端CN──▶ 在 N 个视图 pin ──▶ 确定 (peerID, group)
acceptLoop/handle ──▶ 按 group 路由 inbox / msg 回调
```

### 5.3 对照「one machine one announcer」

决策出处：issue #8 D-7，正文记录在两处代码注释——`internal/discovery/client.go:79-83`
（NoAnnounce 字段）与 `internal/cli/meshcmd.go:86-91`（one-shot 客户端默认 NoAnnounce）；
服务端 `Node.Register` 的守卫是第二层（`client.go:82-83` 指明）。

**决策原文的含义**：一台机器上**同一个 DeviceID** 只能有一个 announcer。one-shot 命令
（`mesh ping`）若也宣告，会以**空端点集合**覆盖长驻 join 进程宣告的真实端点，让这台机器
在所有其他成员的视图里掉线一个心跳周期。防的是**同机、同身份、多进程、端点知识不齐**
的覆盖竞争。

**多宿主不违反，四条理由**：

1. **同进程**：N 个 announcer 在一个进程里，生命周期同生共死，不存在「短命 announcer
   覆盖长命 announcer」——D-7 的场景在拓扑上不成立。
2. **不同身份**：N 个 DeviceID 互不相同（不同密钥，§3.1），锚点按 DeviceID 索引
   presence（`internal/discovery/client.go:327-331` 的 announce 携带各自 ID），各自
   覆盖各自的槽位，互不干扰。
3. **端点集合同源**：N 个身份宣告的端点都解析到**同一个共享监听器**的绑定结果
   （`Listener.BoundAddrs`，`internal/mesh/direct.go:500-509`）——不存在「某个身份
   宣告了别人不知道的端口」，端点知识天然对齐。
4. **one-shot 命令的守卫语义不变**：`mesh ping --state <root>` 仍走 NoAnnounce 路径
   （`internal/cli/meshcmd.go:102`），多宿主下只是把这个规则应用到每个群组的身份上。

**决策的本意记录**（防误读）：D-7 要维护的不变量是「每台机器每个身份恰好一个 announcer，
且它掌握最全的端点知识」。单进程多身份满足该不变量；违反它的是「再起一个进程宣告同一
身份」，这在本设计下依然被禁止（同一 `<root>/groups/<名>` 同时跑两个 join 进程应被
文件锁拒绝，§9 阶段一包含这一守卫）。

---

## 6. 命令面

### 6.1 join

```
# 单群组（今天形态，完全不变）
cmesh join --state /srv/cmesh --cred cmesh1.… --listen 0.0.0.0:0

# 多宿主：同一 state root 下再加一个群组（第二个进程不启动，同一进程承接）
cmesh join --state /srv/cmesh --group team-a --cred cmesh1.… --listen 0.0.0.0:0
cmesh join --state /srv/cmesh --group team-b --cred cmesh1.… --listen 0.0.0.0:0
```

语义：

- `--group` 缺省 = `default`（旧布局兼容）；
- 多宿主下重复执行 `join --group <已有群组>` = **幂等重入**：若该群组已在本进程运行，
  提示已在网；若不在（进程重启后补挂），加载该群组身份并入网——join 进程本身就是
  常驻进程，N 个群组在同一个 `runJoin` 的 N 个 goroutine 中各自跑 §2.4 的编排；
- `--relay-trunk/--relay-trunk-id` 是 per-group flag：每群组可以各自决定是否挂干道
  （`internal/cli/discovery.go:745-752` 的编排按群组各起各的）；
- `--listen` 是**进程级** flag：只给第一个启动的群组（或任一次显式给出），后续群组共享
  该监听器；不给则全组纯出站（各群组各自的 trunk 可达性不受影响）；
- 并发守卫：对 `<root>/groups/<名>/join.lock` 加文件锁（flock），同群组双进程直接拒绝——
  这是 D-7 不变量在多宿主下的执行点（§5.3）。

### 6.2 status

```
cmesh status --state /srv/cmesh          # 列出所有群组
```

输出形态（阶段一为多行文本，阶段二表格化）：

```
group    account             device            online  members  endpoints
team-a   abc23456…           def23456…         yes     7        203.0.113.5:40001
team-b   bbc23456…           cda23456…         no      12       —（trunk-only）
default  （旧布局单群组）    …                 yes     3        …
```

实现：枚举 `<root>/groups/*`（旧布局合成一行 `default`），每群组按
`openMeshSessionWithSnapshotTimeout` 的同构逻辑（`internal/cli/meshcmd.go:50-109`）
取一次 verified snapshot——one-shot、NoAnnounce，与今天的 status 相同
（`internal/cli/status.go:60-98`）。`--group <名>` 过滤单群组；`--probe` 语义不变
（probe 指定的 device ID 在哪个群组的视图里就按哪个群组拨）。

### 6.3 其余命令

`mesh ping/dial/msg/send-to/run/bench`、`msg`、`forward` 等 one-shot 命令统一加
`--group`（缺省 `default`）：只影响读哪个 device.json/discovery.json
（`internal/cli/meshcmd.go:55-75` 的路径解析换成 `resolveGroupDir`），出站身份按 §3.2
天然正确。不带 `--group` 且 `<root>` 是旧布局时行为与今天逐字节一致。

---

## 7. 资源

**共享项**：1 个进程、1 个 UDP 监听 socket（`--listen` 一次）、1 份 Go runtime。

**每群组增量**：

| 项 | 量级 | 依据 |
|---|---|---|
| discovery.Client 状态 + members 流缓冲 | ~0.5–1 MB `[推断]` | Client 结构为几个 map/channel（`client.go:137-201`），wsconn 流缓冲为大头 |
| snapshot 视图 | 与成员数线性，几十成员 ~几十 KB `[推断]` | `MemberSnapshot.Members` |
| trunk join 连接（可选） | 每群组 1 条 QUIC 连接，空闲 ~百 KB 级 `[推断]` | `relaytrunk.go:2711-2753` 常驻一条连接 |
| heartbeat 流量 | 每 30 s 一次 register POST + 视图推送 | `member.go:36`、`client.go:318-341` |

**估算式**：

```
RSS(N) ≈ base + N × (0.7–1.5 MB) + 活跃流量的窗口峰值
base   ≈ 6–9 MB [实测]（单进程空闲基线，docs/ROUND-LOG.md:535）
```

N=5 ≈ 10–17 MB，对比今天 5 个进程的 30–45 MB（5 × 6.3–9.0 `[实测]`）。
验收口径沿用既有 KPI 约束：空闲态采样、不把 bench 峰值当基线（T-5.4，
`docs/ROUND-LOG.md:520` 的教训——常驻 RSS 必须区分「含负载峰值」与「稳态」）。

**连接数**：discovery N 条 WS + 可选 N 条 trunk QUIC（出站），入站监听 1 个 socket。
与今天 N 进程形态相比，节省的只有 socket 与进程开销；discovery 会话数不变——这是有意
的：**每群组一条客户端是信任模型的要求**（各账号的快照各自验证，`client.go:932-986`），
不做跨群组共享视图。

---

## 8. 风险与边界（如实分析）

### 8.1 inbox 按群组分目录

`ListenerConfig.InboxDir` 今天是单一目录（`internal/mesh/direct.go:380-383`）。共享监听器
后入站文件必须落到**对端所属群组**的 inbox。方案：pin 确定群组后把群组名传入会话层——
`serveSession` 已以 peerID 为参数逐层传递（`internal/mesh/direct.go:605-641`），增加
group 参数即可；`receiveFile` 用 `<root>/groups/<group>/inbox/`。回退：`default` 群组
用旧布局的 `<root>/inbox/`。消息回调 `OnMessage` 同样带 group 前缀打印
（`📩 [team-a] from …`）。

### 8.2 cred revoke 的爆炸半径 = 单群组

D77 的 `cred revoke` 级联删「该凭证签的全部设备」（`docs/decisions.md:1350,1355`）。
多宿主下各群组的设备证书由**各自账号/凭证**签发（`internal/cli/join_cred.go:60-77`，
cert 由 cred 的 delegator 铸造、TTL 不越过凭证），吊销群组 A 的凭证只影响 A 名册里的
设备。B 群组的成员视图、members 流、trunk 连接均不感知——它们连的是另一个锚点。
**边界如实记录**：若同一台对端机器也在两个群组各有身份，吊销 A 只踢掉它在 A 的身份，
它在 B 的身份不受影响——这正是网主要的多宿主语义。

### 8.3 N 个身份共享同一 announce 端点：按群组区分的可达性会不会失效？

**结论：可达性不失效，失效的只是「端点隐私按群组区分」这一项。** 分析：

- **可达性为什么不受影响**：宣告的端点指向**本机的共享监听器**。群组 A 的成员拨这个
  端点，到达的是我们的 socket；对端证书由 A 的视图 pin，出示的证书按 SNI/路由选 A 的
  身份——整条链路不经过群组 B 的任何基础设施。群组 B 的成员拨同一端点同理，pin 用 B
  的视图。**端点的可达性跟「机器+端口」走，身份的合法性跟「对端视图」走，两者正交。**
- **D-1 类防护不变**：wildcard 端点仍不宣告（`internal/mesh/direct.go:500-509`）；
  `--endpoint` 显式指定时仍是进程级事实（这台机器的公网/NAT 映射），对所有群组同值。
- **真正失效的边界（如实写）**：
  1. **端点隐私**：无法对群组 A 宣告端点 X、对群组 B 隐瞒 X。B 的所有成员（以及能读到
     B 快照的锚点）都会看到这台机器的地址。若两个群组的信任域不同（例如一个是外部
     合作方群组），需要 per-group 端点宣告（`ClientConfig.Endpoints` 已是 per-client
     字段，`client.go:73-76`，机制上支持；阶段二提供 `--endpoint-per-group` 覆盖，
     默认不区分）。
  2. **群组相关的 NAT 出口**：理论上若两个群组要求从不同源端口/源地址出站（多 WAN
     策略路由场景），单进程单 socket 做不到按群组选源——这属于今天单进程形态的固有限制
     （拨号 socket 绑定通配，`internal/mesh/direct.go:1071`），不是多宿主引入的回归；
     需要时仍可回落「该群组单独跑一个进程」。
  3. **同一身份进多群组**：本设计按「每群组一个身份」设计。若操作者手动把同一张
     device.json 复制进两个群组目录，两个 DeviceID 相同、公钥相同——§3.3 第 3 条的
     pin 分析表明握手仍正确（同一公钥在两个视图都有效），但**吊销隔离被破坏**（账号 A
     无法通过换证书把该设备从 A 踢掉而不影响 B 的同一身份）。处理：join 时检测两个
     群组目录的 device.json 公钥相同 → 拒绝并提示（阶段一实现，代价 ~20 行）。

### 8.4 日志

join 的 stderr 输出与 `slog` 日志全部带群组前缀：`slog` 用 `logger.With("group", name)`
（Client 已接受注入 Logger，`client.go:132-133`）；stderr 直打的行（`discovery.go:541`
等）阶段一统一改成 `[<group>] …` 前缀，避免 N 个群组的输出混流后不可读。`status`
按群组分块输出。

### 8.5 其他风险

| 风险 | 处置 |
|---|---|
| N 个群组共用一个进程：一个群组的 panic 拖垮全部 | join 编排每群组 recover 包裹（与今天 goroutine 纪律一致，T-2.9）；trunk join 已是「失败重试不致命」（`internal/cli/discovery.go:1727-1745`） |
| 口令管理复杂化 | 阶段一：`--device-passphrase` 作用于全部涉及身份；阶段二：`--passphrase-file` 或每群组独立口令的交互提示 |
| GetCertificate 回调阻塞影响握手 | 表查询为内存 map（N 很小），无 IO；启动时预构建 |
| 视图交叉污染 | 禁止：每群组 snapshot 独立验证（各自 AccountPub，`client.go:932-936`），代码层不做任何跨群组合并 |

---

## 9. 分阶段实施

### 阶段一：多身份 join + 各自 discovery + 共享监听（可用）

| 改动 | 文件 | 内容 | 估计行数（生产/测试） |
|---|---|---|---|
| state 解析 | `internal/cli/stategroups.go`（新） | `resolveGroupDir(root, group)`、群组名校验、旧布局探测、join.lock | ~120 / ~150 |
| 监听器多身份 | `internal/mesh/direct.go` | `ListenerConfig` 增加 `Identities map[identity.DeviceID]identity.Device` + `GroupOf` 回调；`NewListener` 改用 `GetCertificate`；pin 改多 resolver 依次尝试；`serveSession` 传 group | ~180 / ~250 |
| 拨号端 SNI | `internal/mesh/direct.go` | `deviceTLS` 增加 ServerName = 目标 DeviceID（可选参数，单宿主路径不传） | ~20 / ~40 |
| join 编排多群组 | `internal/cli/discovery.go` | `runJoin` 抽出 `runOneGroup(ctx, g)`；共享 listener 构建移出循环；per-group client/resolver/trunk/renew | ~200 / ~200 |
| 命令面 | `internal/cli/meshcmd.go`、`internal/cli/status.go` | `resolveGroupDir` 替换路径；status 枚举 groups | ~80 / ~120 |
| identity 去重守卫 | `internal/cli/` | 两个群组 device.json 公钥相同 → 拒绝 | ~20 / ~40 |

合计：**~620 行生产 / ~800 行测试**（区间 500–700 / 700–900，视 GetCertificate 回归
测试的拓扑数而定）。`internal/identity`、`internal/discovery` 零改动（已是参数化设计）。

**验收（全部真机/真实进程，T-2.10）**：① 两群组一进程，`status` 两行都 online；
② 群组 A 内互 ping `link=direct`，群组 B 经 trunk `link=relay`，互不串扰；
③ `cred revoke` A 的凭证 → A 掉线、B 在线不中断；④ 旧布局单群组回归全绿（不迁移）。

### 阶段二：体验 polish

- `status` 表格化 + `--group` 过滤 + probe 按群组归属自动路由；
- 每群组独立口令的交互提示与 `--passphrase-file`；
- per-group 端点覆盖（`--endpoint-per-group`，§8.3 边界 1 的解法）；
- 帮助文本、`DESIGN.md`/`REQUIREMENTS.md` 编号同步、`decisions.md` 立 D<新> 号
  （本设计稿落地时按 T-0.4 转正）；
- systemd 模板 unit 文档：一个 unit 服务多群组（`ExecStart=cmesh join --state …` 全量
  群组），替代 N 份 unit。

---

## 10. 明确不做

| 不做 | 理由 |
|---|---|
| **跨群组身份联邦 / 单点登录**（一个设备身份被多个账号批准、一次登录进所有群组） | 信任模型要求成员证书由**该群组**的账号或其委派签发（`internal/discovery/client.go:932-986` 的验证以群组账号密钥为锚）；联邦需要跨账号证书链与互认协议，是独立的信任层需求，不是编排问题。本设计严格「每群组一个身份」 |
| 跨群组路由 / 桥接（A 群组的成员经本机 reach B 群组的成员） | 两个群组的视图、加密域互不信任；桥接 = 把本机变成跨信任域的转发器，违背 T-4.1 的信任边界精神 |
| 跨群组共享 discovery 会话 / 合并视图 | 各账号快照各自验证是防伪造成员的根基（`client.go:8-16`），合并即放弃该保证 |
| 自动发现「同账号的其他群组」 | 群组之间没有可验证的公共事实；不做猜测量 |

---

## 附：本稿引用的决策与实测记录

| 编号/记录 | 位置 |
|---|---|
| D76 同端口双 ALPN（裁定 + 库层核实） | `docs/decisions.md:1310-1337` |
| D76 真机实测（单套接字双 ALPN 各连各的） | `docs/ROUND-LOG.md:1429` |
| D77 入网即常驻 / cred revoke / 多群组单机现状 | `docs/decisions.md:1344-1362` |
| issue #8 D-7（one machine one announcer） | `internal/discovery/client.go:79-83`、`internal/cli/meshcmd.go:86-91` |
| 空闲 RSS 基线 / join 常驻 RSS | `docs/ROUND-LOG.md:535`、`:520` |
