# 架构与依赖地图：从命令行入口到数据面

> 本文是**测绘报告**，不是设计稿。它回答一个问题：**当前代码里，从 `cmesh` 的一个子命令到数据面，
> 到底经过哪些包、哪些函数、哪条线协议。**
>
> 位置：三层文档的第三层（`design/` 层）的一个补充。上层是
> [`../REQUIREMENTS.md`](../REQUIREMENTS.md)（要什么）与 [`../DESIGN.md`](../DESIGN.md)（用什么结构），
> 同层的是 [`MESH-DESIGN.md`](MESH-DESIGN.md)（组网详细设计）。
>
> **撰写时的代码基线**：`develop` 分支，HEAD = `96fdef8fdd3c56d5201c3a906872a1d672ac4249`
> （2026-11-19）。工作树有未提交改动，因此本文的 file:line 对该基线成立，
> 对改动后的工作树可能漂移。
>
> 纪律：每条「当前代码是这样」的断言都指到 **file:line**（撰写时逐条核实）。
> 不含虚构的设计、测试结果或性能数字。数字标注遵循 T-5.3：
> `[实测]` / `[查证]` / `[推断]`——本文不做性能断言，只标 `[查证]`（读代码确认）。

---

## 0. 摘要：五层结构与三条主线

```
┌──────────────────────────────────────────────────────────────────────┐
│ 交付层  packaging/（安装器·systemd·发布脚本） · internal/relay 面板  │
│         web/（React+Three.js 驾驶舱，构建产物内嵌）                   │
├──────────────────────────────────────────────────────────────────────┤
│ CLI 层   cmd/cm → internal/cli（唯一入口；23 个子命令分支）         │
├──────────────────────────────────────────────────────────────────────┤
│ 控制面   internal/discovery（客户端·节点·名册快照·邀请·注册）         │
│         internal/relay（阶段 1 配对中转 + /mesh/* 路由 + 面板）       │
├──────────────────────────────────────────────────────────────────────┤
│ 传输层   internal/mesh（直连 QUIC·relay trunk·E2E 封签·路径选择）    │
├──────────────────────────────────────────────────────────────────────┤
│ 身份层   internal/identity（账号·设备·证书·委派·名册）                │
└──────────────────────────────────────────────────────────────────────┘
```

三条主线：

1. **直连优先、中转兜底**（D73）：`ViewPicker` 先给直连路径，再给中转候选。
2. **成员视图是唯一信任锚**：发现节点不被信任为诚实，只被信任为在线；每一份快照的
   证书都被客户端用账号公钥重新验证（`internal/discovery/client.go:932`）。
3. **中转只搬字节**：trunk 的转发循环不解封载荷（`internal/mesh/relaytrunk.go:1212`），
   端到端密钥只有两端持有（`internal/mesh/session.go:330`）。

**依赖图无环**（§5，`go list` 导出核对）。

---

## 1. CLI 层（`internal/cli/`）

### 1.1 入口与派发

```
cmd/cm/main.go:25          cli.Run(os.Args[1:])
  └─ internal/cli/cli.go:48   Run(args) —— 唯一 switch，23 个 case 分支
       ├─ probe      → cli/probe.go:14   runProbe
       ├─ status     → cli/status.go:60  runStatus
       ├─ doctor     → cli/status.go:216 runDoctor（直接委托 runProbe）
       ├─ relay      → cli/relay.go:18   runRelay → serveRelay:82
       ├─ send/recv  → cli/peer.go:169/273   runSend / runRecv（阶段 1 配对）
       ├─ forward    → cli/forward.go:30  runForward
       ├─ init       → cli/mesh.go:60    runInit
       ├─ account    → cli/mesh.go:107   runAccount（create/device-add/authorize/show）
       ├─ show-pub   → cli/mesh.go:193   runShowPub
       ├─ invite     → cli/discovery.go:244  runInvite
       ├─ join       → cli/discovery.go:491  runJoin（含 --cred）
       ├─ cred       → cli/cred.go:25/43/187/275
       ├─ verify     → cli/verify.go:28  runVerify
       ├─ serve-discovery → cli/discovery.go:1001 runServeDiscovery
       ├─ mesh       → cli/meshcmd.go:218 runMesh → :393 runMeshPing
       ├─ dial       → cli/meshcmd.go:497 runDial
       ├─ msg        → cli/meshcmd.go:283 runMsg
       ├─ send-to    → cli/meshcmd.go:344 runSendTo
       ├─ bench      → cli/bench.go:23   runBench
       ├─ run        → cli/meshcmd.go:233 runRun
       └─ version/help
```

退出码契约：`cli.ExitError`（`internal/cli/cli.go:33`）；`cmd/cm/main.go:33` 用
`errors.As` 取它，`context.Canceled` 不打印（`cmd/cm/main.go:39`）。

### 1.2 CLI 层图（只画 mesh 主线相关的；阶段 1 的 send/recv/forward 单独标注）

```
internal/cli
  ├─ meshcmd.go
  │   meshSession{dev, cl, resolver}                :35
  │   openMeshSession()                             :46  → discovery.NewClient
  │   openMeshSessionWithSnapshotTimeout()          :50
  │   (*meshSession).resolveTarget()                :164 → cl.Snapshot().Members[target].Endpoints
  │   (*meshSession).picker()                       :206 → mesh.ViewPicker{View,Self,TargetKey}
  │   runMeshPing/runDial/runMsg/runSendTo/runRun   :393/:497/:283/:344/:233
  │        └→ mesh.DialViaPickerPath / mesh.DialDirect / mesh.SendMsg / mesh.SendFile / mesh.SendRun
  ├─ discovery.go
  │   liveViewResolver                              :376  → mesh.SnapshotAdapter
  │   relayMembersResolver                          :398  → mesh.MemberListAdapter
  │   runJoin                                       :491
  │   runServeDiscovery                             :1001 → relay.New / mesh.NewRelayTrunk / mesh.NewListener
  │   watchApprovals                                :1587（名册热重载）
  │   certRenewLoop                                 :1731（证书续期）
  │   joinRelayTrunk                                :1804 → mesh.JoinRelayTrunk
  ├─ join_cred.go   enrollWithCredential             :54  → POST /mesh/enroll
  ├─ enroll.go      enrollDevice                    :49  → identity.MemberList.Add
  │                 installEnrollHandler            :129 → relay.Server.SetEnrollHandler
  ├─ mesh.go        identity.Generate/GenerateAccount/IssueCert/SaveCert
  ├─ cred.go        credential.Create/Encode/Decode；identity.LoadMembersOrEmpty
  ├─ node.go        阶段 1 的 Node（peer/mux/p2p/p2pquic），与 mesh 主线**不相交**
  ├─ status.go      runStatus/probeMember           :60/:166
  └─ bench.go       runBench                         :23
```

### 1.3 CLI 层的测试 seam（生产恒为 nil）

四个包级变量，都是**测试专用接缝**，生产代码里恒为 `nil`：

| seam | 声明 | 生产读取点 | 测试使用者 |
|---|---|---|---|
| `discoveryClientTransportHook` | `internal/cli/discovery.go:85` | `withDiscoveryClientTransport` `:87`（被 `runJoin` `:670` 与 `openMeshSession` `meshcmd.go:96` 调用） | `cli/discovery_test.go:184`、`cli/join_timeout_test.go:144`、`cli/relaytrunk_test.go:92` |
| `joinResolverHook` | `internal/cli/discovery.go:69` | `runJoin` `:698`（把 join 建出来的 resolver 交出去） | `cli/relaytrunk_test.go:104` |
| `joinLifetimeHook` | `internal/cli/discovery.go:83` | `runJoin` `:768`（替换信号上下文，供测试控制成员进程寿命） | `cli/discovery_test.go:265`、`cli/join_timeout_test.go:306` |
| `certRenewIntervalHook` | `internal/cli/discovery.go:1711` | `certRenewLoop` `:1751`（压缩续期间隔） | `cli/cred_renew_test.go:84` |

> 这些 seam 是**全局可变状态**，不是依赖注入。`internal/cli` 里没有接口把它们参数化，
> 所以每个用它们的测试都必须自己 save/restore（见上表「测试使用者」列）。

---

## 2. 控制面（`internal/discovery/` + `internal/relay/` 的 /mesh/* 部分）

### 2.1 客户端侧

```
internal/discovery
  ClientConfig                                     client.go:63
  Client                                            client.go:137
  NewClient()                                      client.go:205
  (*Client).Run(ctx)                               client.go:517   ← 心跳循环 + 快照流
      ├─ (*Client).Announce(ctx)                   client.go:318
      │     └→ MemberAnnounce.Sign                 member.go:193
      │     └→ postRegister → POST /mesh/register  client.go:349
      ├─ (*Client).streamLoop(ctx)                 client.go:718
      │     └→ streamOnce → WS /mesh/members       client.go:784
      │     └→ verifySnapshot                      client.go:932  ← 每张证书用账号公钥复验
      │     └→ applySnapshot（版本单调）            client.go:993
      ├─ (*Client).Leave(ctx)                      client.go:436  ← LEAVE 广播
      ├─ (*Client).Snapshot()                      client.go:296
      └─ (*Client).Joined()                        client.go:305
  MemberAnnounce{DeviceID,Endpoints,RelayEndpoints,Leave,Session,…}  member.go:59
  (*MemberAnnounce).Validate                       member.go:131
  Invite{AccountID,AccountPub,DiscoveryAddrs}      member.go:346
  EncodeInvite / DecodeInviteWith                  member.go:354 / :395
  EnrollRequest / EnrollResponse                   enroll.go:25 / :34
```

### 2.2 节点侧（锚点）

```
internal/discovery
  Node                                             node.go:19
  NewNode(members)                                 node.go:74
  (*Node).Register(announce, when)                 node.go:145   ← 记录 endpoint / relay endpoint / presence
  (*Node).SetRelayEndpoints                        node.go:293
  (*Node).Sweep()                                  node.go:373   ← 超时成员下线
  (*Node).Subscribe / AttachStream / DetachStream  node.go:334 / :89 / :112
  BuildSnapshot(members, node, now)                snapshot.go:124 ← 每次推送重建全量视图
  MemberSnapshot{AccountID,Version,Members,Delegations}  snapshot.go:83
  MemberView{Cert,Endpoints,RelayEndpoints,LastSeen,Online} snapshot.go:44
  liveDelegations()                                snapshot.go:103 ← 只带未过期的委派

internal/relay（/mesh/* 路由 + 阶段 1 配对同进程）
  (*Server).EnableDiscovery(members)               mesh.go:91
  (*Server).SetMeshMembers / MeshMembers           mesh.go:135 / :146
  (*Server).handleMeshRegister                     mesh.go:276  ← POST /mesh/register
  (*Server).handleMeshMembers                      mesh.go:365  ← WS /mesh/members
  (*Server).handleMeshEnroll                       mesh.go:211  ← POST /mesh/enroll
  (*Server).streamMeshMembers                      mesh.go:400  ← 推送全量快照
  (*Server).SetEnrollHandler                       mesh.go:199
```

路由挂载：`(*Server).Handler()`（`internal/relay/relay.go:292`）——配对 `/ws`
（`internal/relay/relay.go:557`）、`/status`、`/dashboard`，以及 discovery 开启时的 `/mesh/*` 与
`/panel`、`/api/mesh`（`internal/relay/relay.go:305-317`）。

### 2.3 控制面图

```
                 ┌───────────────── 成员进程 ─────────────────┐
                 │                                            │
   POST /mesh/register ──► Client.Announce ──► MemberAnnounce.Sign
                 │      (client.go:318)          (member.go:193)
                 │                                    │
                 │                          ┌─────────▼──────────┐
                 │                          │ 锚点 relay.Server  │
                 │                          │ handleMeshRegister │
                 │                          │ (mesh.go:276)      │
                 │                          │  ├ meshMemberKey   │
                 │                          │  ├ Announce.Validate
                 │                          │  └ Node.Register   │
                 │                          │     (node.go:145)  │
                 │                          └─────────┬──────────┘
                 │                                    │ notifyAll
   WS /mesh/members ◄── streamMeshMembers ◄── BuildSnapshot (snapshot.go:124)
                 │        (mesh.go:400)            MemberSnapshot
                 │
                 └─► Client.verifySnapshot (client.go:932) → applySnapshot (client.go:993)

   POST /mesh/enroll ──► handleMeshEnroll (mesh.go:211) ──► enrollHook
                          └─► cli/enroll.go:49 enrollDevice ──► identity.MemberList.Add
```

---

## 3. 传输层（`internal/mesh/`）

### 3.1 直连 QUIC

```
internal/mesh
  quicConfig()                                     direct.go:308  ← 唯一的 quic.Config 构造点
  deviceTLS(cert, pin)                             direct.go:346  ← InsecureSkipVerify + VerifyConnection 精确 pin
  ListenerConfig / Listener / NewListener          direct.go:380 / :420 / :434
  (*Listener).handle(conn)                         direct.go:574  ← 按 ALPN 分流
  (*Listener).serveSession                         direct.go:623
  serveSession(kind, peerID, br, st, cfg)          direct.go:677  ← kind → handler 的唯一映射
  DialConfig / DialDirect                          direct.go:925 / :948
  dialOne / dialOneRaw                             direct.go:1073 / :1081
  MemberResolver / MemberListAdapter / SnapshotAdapter  direct.go:184 / :191 / :221
  SendMsg / SendFile / SendRun                     direct.go:1174 / :1223 / exec.go:211
  PingExchangeStats                                direct.go:1394
  receiveFile（sha256 校验后落盘）                 direct.go:830
  echoPings                                        direct.go:893
  DialRelaySession                                 direct.go:1516 ← ALPN = RelayALPN
```

### 3.2 relay trunk（中转干道）

```
internal/mesh/relaytrunk.go
  RelayOpen / RelayAnswer                          :102 / :123
  relayRecord（JSON 信封，Data 是不透明密文）      :569
  writeRelayRecord / readRelayRecord               :599 / :644
  RelayTrunk / NewRelayTrunk                       :234 / :286
  (*RelayTrunk).Decide(open)                       :354   ← 授权状态机（成员→开关→自环→跳数）
  (*RelayTrunk).OpenFlow                           :933   ← 复检成员身份 + 配额 + 拨目标
  (*RelayTrunk).splice / forwardRecords            :1184 / :1212 ← 逐字节转发，不解封
  RelayQuota / DefaultRelayMaxStreams / …BytesPerSec :697 / :686 / :689
  OpenRelay(ctx, RelayRequesterConfig)             :1616 ← 请求方：open→answer→E2E 握手
  relaySession（请求方视图：Write 封签 / Read 解封）:1484 / :1511 / :1528
  JoinerTable / TrunkJoiner                        :1903 / :1829
  (*RelayTrunk).ServeRequesterWithFirst            :2064 ← 干道侧：判权→开流→转发→等 flow 结束
  ServeRelayTarget                                 :2340 ← 目标侧：验 hello→出自己的 hello→交出 Session
  JoinRelayTrunk / joinRelayTrunkOnce              :2778 / :2798
  serveRelayedSession                              :2848
  RelayALPN = "container-mesh-relay/1"             :2442（与 directALPN "container-mesh-direct/1" direct.go:377 分离）
  SetRelayTrunk / InstalledRelayTrunk              :445 / :453 ← 进程级单例
```

E2E 封签在 `internal/mesh/session.go`：

```
  Session 接口（Close 是半关闭）                    session.go:53
  E2EHello / Transcript                            session.go:206 / :230
  NewE2ESession                                    session.go:330
  (*E2ESession).AcceptHello                        session.go:416
  (*E2ESession).Seal / Open                        session.go:541 / :559
  e2eKeys（方向绑进派生）                            session.go:474
```

### 3.3 路径选择

```
internal/mesh/route.go
  PathKind{PathDirect,PathRelay}                   :31-38
  Path{Kind,Via,Endpoints,ViaEndpoints,Hops,TargetKey} :41
  PathPicker 接口                                   :84
  DirectFirstPicker.Pick                           :103
  tryPaths（Auth/Refused 立即停，其余换下一条路）     :139
  DialViaPickerPath（返回实际用的 Path）             :189
  dialPath（direct→DialDirect；relay→dialViaRelay）  :252
  dialViaRelay                                     :290
  ViewPicker.Pick（直连优先 + 中转候选）             :404
internal/mesh/candidates.go
  RelayCandidates(view, target, self)              :43  ← 按 DeviceID 排序，确定性
```

### 3.4 传输层图

```
   ViewPicker.Pick (route.go:404)
        │
        │  Path[0]=direct, Path[1..n]=relay(Via,ViaEndpoints,TargetKey)
        ▼
   tryPaths (route.go:139)
        │
        ├─ PathDirect ─► DialDirect (direct.go:948)
        │      ├─ quic.Dial + quicConfig (direct.go:1100 / :308)
        │      ├─ deviceTLS: mTLS + 精确 pin 到成员视图 (direct.go:346)
        │      ├─ Hello{Proto,Kind,Time} → "ok"  (direct.go:1123 / :646)
        │      └─ Link{sess=quicSession}  → serveSession (direct.go:677)
        │             ├─ KindPing → echoPings (direct.go:893)
        │             ├─ KindMsg  → receiveMsgs (direct.go:731)
        │             ├─ KindFile → receiveFile (direct.go:830)  sha256→落盘
        │             ├─ KindRun  → receiveRun (exec.go:91)
        │             └─ KindBench→ receiveBench
        │
        └─ PathRelay ─► dialViaRelay (route.go:290)
               ├─ 目标 key 取自成员视图（不成员 → ErrAuth，不再换路）
               ├─ DialRelaySession (direct.go:1516)  ← 到干道的 QUIC，ALPN=RelayALPN
               └─ OpenRelay (relaytrunk.go:1616)
                     ├─ ① 写 open 记录  → 干道 Decide (relaytrunk.go:354)
                     │                  → 干道 OpenFlow (relaytrunk.go:933)
                     │                     └─ forwardRecords (relaytrunk.go:1212) 逐记录转发
                     ├─ ② 读 answer 记录（grant / class）
                     ├─ ③ E2E 握手穿透干道：sendHello → acceptHello (relaytrunk.go:1723/1747)
                     └─ 返回 relaySession（Write 封签 / Read 解封）→ 业务层无感
```

---

## 4. 身份层（`internal/identity/`）

```
internal/identity（**不依赖任何项目内包**）
  DeviceID / IDFromKey                            identity.go:60 / :78
  Device{priv,pub,id} / Generate / LoadDevice      identity.go:84 / :97 / :252
  (*Device).TLSCertificate（自签，CN = DeviceID）  identity.go:137
  Account / GenerateAccount / LoadAccount          account.go:48 / :60 / :69
  DeviceCert{Cert,DevicePub,SignerPub,AccountID,…} account.go:102
  DefaultCertTTL = 365d                            account.go:124
  (*Account).IssueCert / IssueCertForPub           account.go:139 / :149
  SignDelegatedCert（委派者签）                     account.go:196
  (*DeviceCert).Expired / VerifySigner / Verify    account.go:240 / :248 / :273
  Delegation{ID,AccountID,DelegatorPub,MaxDevices,ExpiresAt,Sig} delegation.go:49
  (*Account).SignDelegation / (*Delegation).Verify delegation.go:87 / :110
  Member{Cert,AddedAt,LastSeen}（**刻意没有 Endpoints**） members.go:33
  MemberList{AccountID,Version,Members,Delegations,RevokedCreds} members.go:49
  (*MemberList).Add / Remove / Contains / Get      members.go:343 / :366 / :379 / :387
  (*MemberList).AddDelegation / RevokeDelegation    members.go:99 / :136
  (*MemberList).RevokeCredential / CredentialRevoked members.go:205 / :265
  (*MemberList).SaveMembers / SaveMembersMerged     members.go:433 / :456
  LoadMembers / LoadMembersOrEmpty / repairRosterShape members.go:572 / :656 / :610
```

信任链一句话：**账号签设备证书（`identity/account.go:139`），名册记「谁属于这个网络」
（`identity/members.go:49`），成员视图把证书带给每个客户端复验（`discovery/snapshot.go:143`），
客户端用账号公钥复验（`discovery/client.go:955`），直连时用视图里的 key 精确 pin 证书公钥
（`mesh/direct.go:986`）。**

---

## 5. 依赖方向图

### 5.1 项目内包依赖（`go list` 导出核对）

```
internal/identity   → （无项目内依赖）           ← 底层
internal/wire       → （无项目内依赖）
internal/wsconn     → （无项目内依赖）
internal/version    → （无项目内依赖）
internal/cmeshtest  → （无项目内依赖）
internal/mux        → （无项目内依赖）
internal/probe      → （无项目内依赖）
internal/p2pquic    → （无项目内依赖）
internal/secure     → wire
internal/credential → identity
internal/discovery  → identity, wsconn
internal/relay      → discovery, identity, wsconn
internal/mesh       → discovery, identity, wire, version
internal/peer       → secure, wire
internal/forward    → mux, wire
internal/p2p        → wire
internal/cli        → identity, discovery, mesh, relay, credential, forward,
                      peer, secure, mux, p2p, p2pquic, probe, wire, wsconn,
                      version, cmeshtest
cmd/cm              → cli
tools/cmesh-install, tools/mkrelease → （无项目内依赖）
```

### 5.2 分层图

```
cmd/cm
  └── internal/cli
        ├── internal/identity ────────────┐
        ├── internal/discovery ───────────┤
        │      └── internal/identity ─────┤
        ├── internal/mesh ────────────────┤
        │      ├── internal/discovery ────┤
        │      ├── internal/identity ─────┤
        │      ├── internal/wire ──────────┤
        │      └── internal/version ──────┤
        ├── internal/relay ───────────────┤
        │      ├── internal/discovery ────┤
        │      ├── internal/identity ─────┤
        │      └── internal/wsconn ───────┤
        └── internal/credential ──────────┘
              └── internal/identity
```

### 5.3 是否有环

**无环。** [查证] 依据：`go list -f '{{.ImportPath}}: {{join .Imports "\n  "}}' ./internal/...` 的输出
（§5.1 逐条抄录）。观察到的依赖是**分层单向**的：`identity` 在最底，`discovery` 在其上，
`mesh` / `relay` 再上一层，`cli` 在最顶。图上不存在回边。

### 5.4 禁止的依赖（当前均未发生）

| 禁止方向 | 理由 | 当前状态 |
|---|---|---|
| `internal/identity` → `internal/mesh` | 身份层不该知道传输 | ✅ 未发生（`identity` 只 import 标准库 + `golang.org/x/crypto/argon2`） |
| `internal/identity` → `internal/discovery` | 名册是身份的产物，不是发现的产物 | ✅ 未发生 |
| `internal/discovery` → `internal/mesh` | 控制面不该依赖数据面 | ✅ 未发生（`discovery` 只 import `identity` + `wsconn`） |
| `internal/discovery` → `internal/relay` | 同上，且会成环 | ✅ 未发生 |
| `internal/mesh` → `internal/relay` | 干道不依赖阶段 1 配对中转 | ✅ 未发生 |
| `internal/relay` → `internal/mesh` | 阶段 1 配对中转与 mesh 干道是两个东西 | ✅ 未发生（`relay` 只 import `discovery`/`identity`/`wsconn`） |
| `internal/wire` / `internal/mux` → 任何项目内包 | T-3.10 的明文要求 | ✅ 未发生 |

唯一「反向」的依赖是 `internal/mesh` → `internal/discovery`：`mesh` 需要
`discovery.MemberSnapshot` 做路径选择与 pin（`mesh/route.go:394`、`mesh/direct.go:222`、
`mesh/candidates.go:43`）。这是**控制面读接口**，不是反向控制流，方向仍然单一。

---

## 6. 关键数据流（端到端）

### 6.1 一次 `cmesh mesh ping <deviceID>`

```
[CLI]
 runMeshPing                         cli/meshcmd.go:393
  └ openMeshSession(ctx,state,pass,nil)          cli/meshcmd.go:46
      ├ identity.LoadDevice(devPath, pass)       identity/identity.go:252
      ├ 读 <state>/discovery.json → discoveryConfig.invite()  cli/discovery.go:337
      ├ discovery.NewClient(ClientConfig{NoAnnounce: len(known)==0})  discovery/client.go:205
      └ 等第一份已验证快照（15s 预算）           cli/meshcmd.go:114-144
  └ sess.resolveTarget(ctx, target)              cli/meshcmd.go:164
      └ cl.Snapshot().Members[target].Endpoints  discovery/client.go:296
  └ sess.picker() → mesh.ViewPicker{View: cl.Snapshot, Self, TargetKey}  cli/meshcmd.go:206
  └ mesh.DialViaPickerPath(DialConfig{...})      mesh/route.go:189
      ├ Resolver.KeyFor(target)（不成员 → ErrAuth，不再换路）  mesh/route.go:197
      ├ ViewPicker.Pick                           mesh/route.go:404
      │    ├ Path[0] = {Kind: PathDirect}
      │    └ Path[1..] = {Kind: PathRelay, Via, ViaEndpoints, TargetKey}
      │         候选来自 RelayCandidates（按 DeviceID 排序） mesh/candidates.go:43
      └ tryPaths → dialPath                       mesh/route.go:139 / :252
           ├─ 直连：DialDirect                    mesh/direct.go:948
           │     quic.Dial(+quicConfig)           mesh/direct.go:1100 / :308
           │     TLS：deviceTLS，pin 到成员视图的 key  mesh/direct.go:346 / :986
           │     Hello{Proto:"cmesh-mesh/1",Kind:"ping"}  mesh/direct.go:1123
           │     ← "ok"                           mesh/direct.go:1137
           │   或
           └─ 中转：dialViaRelay                  mesh/route.go:290
                 OpenRelay                        mesh/relaytrunk.go:1616
                   ① open 记录 → 干道 Decide → OpenFlow → forwardRecords  mesh/relaytrunk.go:354/933/1212
                   ② answer 记录（grant）
                   ③ E2E 握手穿透干道（SIGMA）：sendHello / acceptHello  mesh/relaytrunk.go:1723/1747
                      对端：ServeRelayTarget → relayTargetSession  mesh/relaytrunk.go:2340
  └ mesh.PingExchangeStats(ctx, link, count, interval)   mesh/direct.go:1394
      记录 seq+u64 发送时刻（pingRecordSize=16，direct.go:53）→ 回读匹配 → RTT
  └ 打印 p50/p95 + link=direct|relay via=<deviceID>   cli/meshcmd.go:468-487
      used.Kind == mesh.PathRelay → "relay via=…"  cli/meshcmd.go:480
```

**接收侧**：直连 → `serveSession(KindPing)` → `echoPings`（`mesh/direct.go:893`）；
中转 → 目标侧 `serveRelayedSession` → `mesh.ServeRelayedSession`（`mesh/direct.go:665`）
→ 同一个 `serveSession`（`mesh/direct.go:677`）。**业务层无法区分直连与中转。**

### 6.2 一次 `cmesh join --cred <credential>`

```
[CLI]
 runJoin(credStr != "")              cli/discovery.go:491
  ├ credential.Decode(credStr)       credential/credential.go:197
  ├ cred.Verify(accountPub, now)     credential/credential.go:262
  ├ ensureDeviceIdentity(dir, pass)  cli/join_cred.go:144
  │    └ identity.Generate + Device.Save   identity/identity.go:97 / :212
  ├ enrollWithCredential(ctx, cred, dev)   cli/join_cred.go:54
  │    ├ cred.MintCert(devPub, ttl, now)   credential/credential.go:291
  │    │    └ identity.SignDelegatedCert   identity/account.go:196
  │    ├ discovery.EncodeEnrollRequest     discovery/enroll.go:51
  │    └ POST <base>/mesh/enroll           cli/join_cred.go:106
  │         （传输受 discovery.EgressTransportWith 的出站地址策略约束
  │           cli/join_cred.go:97）
  │
  └ [锚点侧] relay.Server.handleMeshEnroll   relay/mesh.go:211
        └ enrollHook → cli/enroll.go:49 enrollDevice
             ① 撤销名单优先：members.CredentialRevoked(pub.ID)   cli/enroll.go:64
             ② pub.Verify(accountPub, now)                        cli/enroll.go:69
             ③ 证书签名者 == 委派者公钥                             cli/enroll.go:77
             ④ cert.AccountID == pub.AccountID                    cli/enroll.go:80
             ⑤ cert.VerifySigner(pub.DelegatorPub)                cli/enroll.go:83
             ⑥ 证书未过期                                          cli/enroll.go:86
             ⑦ 配额：members.DelegatedCount(pub.DelegatorPub)      cli/enroll.go:90
             ⑧ 名册属于同一账号                                    cli/enroll.go:95
             ⑨ members.AddDelegation(pub)（委派记录进名册）         cli/enroll.go:106-116
             ⑩ members.Add(cert)（设备进名册，Version++）           cli/enroll.go:117
        └ live.SaveMembersMerged(membersPath)                     cli/enroll.go:164
        └ srv.SetMeshMembers(live)  → 触发新视图推送             cli/enroll.go:168
  │
  └ [客户端侧] 回到 join：
       inv = inviteFromCredential(cred)          cli/join_cred.go:162
       ├ （可选 --listen）mesh.NewListener(...)   cli/discovery.go:627
       ├ discovery.NewClient(...)                cli/discovery.go:670
       ├ resolver.set(cl)                        cli/discovery.go:695
       ├ saveDiscoveryConfig(dir, cfg)           cli/discovery.go:727
       ├ go cl.Run(runCtx)                       cli/discovery.go:815  ← 心跳 + 成员流
       ├ go certRenewLoop(runCtx, credJoined, dev, nil)  cli/discovery.go:827
       └ （可选 --relay-trunk）go joinRelayTrunk(...)      cli/discovery.go:843
```

**announce → 成员流**：`Client.Run`（`discovery/client.go:517`）先发一次
`Announce`（`:318` → `POST /mesh/register`），锚点 `Node.Register`（`discovery/node.go:145`）
记录 endpoint / relay endpoint 后 `notifyAll()`，成员流把新快照推给每个订阅者
（`relay/mesh.go:400` → `BuildSnapshot` `discovery/snapshot.go:124`），客户端
`verifySnapshot`（`discovery/client.go:932`）复验后 `applySnapshot`（`:993`），
`cl.Joined()` 变 true，join 打印 "approved"（`cli/discovery.go:920-923`）。

**成员流的信任门**：非成员在 upgrade 之前就被拒（`relay/mesh.go:366-374`）；
流凭证是 `MeshAuthContext + deviceID + timestamp` 的成员签名（`discovery/snapshot.go:25`）。

### 6.3 一次 `cmesh send-to --file PATH <deviceID>`（经中转）

```
[CLI]
 runSendTo                          cli/meshcmd.go:344
  ├ openMeshSession / resolveTarget / picker      （同 §6.1）
  └ mesh.SendFile(ctx, DialConfig{...}, path)     mesh/direct.go:1223
      ├ 打开文件 → 算 sha256 → FileHeader{Name,Size,SHA256}   mesh/direct.go:1236
      ├ cfg.Kind = KindFile
      ├ dialViaPicker → 路径选择                   mesh/route.go:212
      │   （直连失败/不可用时走 PathRelay）
      ├ 写 FileHeader 一行 → 等 "ok"               mesh/direct.go:1260 / :1266
      │    接收侧先判 inbox 与大小上限，拒绝得早     mesh/direct.go:839-855
      └ sendFileBody（1 MiB 分块，60s 滚动写截止）  mesh/direct.go:1292

[中转路径的额外步骤]
  OpenRelay                         mesh/relaytrunk.go:1616
   ├ 写 open 记录 {From,To,Hops:1,Kind:"file"}      mesh/relaytrunk.go:1671
   ├ 干道 Decide → grant                              mesh/relaytrunk.go:354
   ├ 干道 OpenFlow：复检两端成员身份 → 拨目标 → 建 flow  mesh/relaytrunk.go:933
   ├ 干道把 open 记录转给目标（目标才知道是谁在呼叫）  mesh/relaytrunk.go:2144
   ├ 目标侧 serveRelayedSession → ServeRelayTarget（验 hello、出 hello）mesh/relaytrunk.go:2848/2340
   ├ 握手完成后业务层接管：relaySession.Write 封签 → 干道逐记录转发   mesh/relaytrunk.go:1511/1212
   └ 目标侧 receiveFile                              mesh/direct.go:830
        ├ 读 FileHeader，校验 name/size              mesh/direct.go:831-857
        ├ 写临时文件 .incoming-*，边写边算 sha256      mesh/direct.go:865-871
        ├ 比对 h.SHA256 → 不符则删临时文件并报错      mesh/direct.go:873-877
        ├ os.Rename 落盘到 <inbox>/<base>            mesh/direct.go:878-881
        └ 写 "done <sha256>" → 请求方读到后判定成功     mesh/direct.go:883
  请求方校验 verdict 前缀 "done"                     mesh/direct.go:1281
  CLI 打印 "sent <name> (<size> bytes) … sha256 <hex> — receiver verified"  cli/meshcmd.go:388
```

**sha256 的方向**：发送方先算好并放进 header，接收方边收边算、**不符就删文件**。
所以这个 sha256 是「接收方承诺校验的那个值」，不是事后补算的。

---

## 7. 交付层

### 7.1 打包与发布

```
packaging/
  install.sh          POSIX 安装器：<install>/versions/<v>/cmesh + current 软链（install.sh:5-9）
  uninstall --purge   只清 CMESH_STATE
  build-release.sh    交叉编译（静态、CGO-free）、版本号 ldflags 注入、版本名白名单校验
  start.sh / stop.sh  “解压即跑”的一键中继脚本（POSIX sh，无依赖）
  systemd/cmesh-discovery.service
      EnvironmentFile=/etc/cmesh/discovery.env（必需，缺文件必须响亮失败）
      LoadCredential=panel-token:/etc/cmesh/panel-token（面板令牌不进命令行也不进环境变量）
      ExecStart=… cmesh serve-discovery --state … --addr … --announce …
               --account-pub … --relay-trunk --panel --panel-token-file …
      KillSignal=SIGTERM / TimeoutStopSec=15s（配合 serveDiscoveryGrace=10s 的排空）
      UMask=0077 / NoNewPrivileges / PrivateTmp / ProtectSystem=strict / ProtectHome=read-only
  systemd/cmesh-discovery.env.example
```

`Makefile`：`build` 用 `-trimpath -ldflags "-s -w -X …/internal/cli.Version=$(VERSION)"`；
`check` 跑 `gofmt -l .` + `go vet ./...`。

### 7.2 面板（驾驶舱）

```
internal/relay
  panel.html                  //go:embed 内嵌的轻量面板静态壳（无数据）  panel.go:31
  cockpit-dist/               //go:embed all:cockpit-dist —— web/ 的构建产物  assets.go:25
  assets.go:29 cockpitFS      assets.go:53 (*Server).handleCockpit
  panel.go:83  PanelData / :46 PanelMember / :77 PanelSummary
  panel.go:164 (*Server).handleAPIMesh   ← 令牌保护的数据端点
  panel.go:316 (*Server).panelAuthorized（crypto/subtle 常量时间比较）
  cockpit_model.go:35 CockpitZone / :49 CockpitMember / :81 CockpitContainerConfig / :97 CockpitPayload
  cockpit_model.go:124 buildCockpitPayload（从 PanelMember 行构造驾驶舱视图模型）
```

令牌解析顺序（`cli/discovery.go:177` `resolvePanelTokenFrom`）：**文件 → 命令行 → 环境变量 → 生成**。
文件排第一的理由写在 `cli/discovery.go:161-176`：命令行令牌可被同机用户从
`/proc/<pid>/cmdline` 读到，环境变量可被同用户的任意进程读到。

驾驶舱的建模原则写在 `internal/relay/cockpit_model.go:6-27`：只有一个 zone（这个部署本身）、
设备形状由可达性决定、地理**报「未登记」而不是编造**、容器指纹**报「未采集」**。

### 7.3 文档

`docs/` 三层：`REQUIREMENTS.md`（需求正典）→ `DESIGN.md`（结构）→
`design/`（怎么做）→ `research/`（实测依据）；`decisions.md` 记 `D<n>` 决策史；
`CONVENTIONS.md` 是约束（本文遵循其中的 T-3.10 无环依赖、T-5.3 数字标注、
T-5.5 文档定位声明）。

---

## 8. 改动影响面表

| 改动点 | 会波及什么 |
|---|---|
| `internal/mesh/relaytrunk.go` 的 `relayRecord` 记录格式（`:569`） | 干道与两端共 3 处编解码：`writeRelayRecord`/`readRelayRecord`（`:599`/`:644`）、请求方 `relaySession.Read`（`:1528`）、目标侧 `relayTargetSession.Read`（`:2258`）；`maxRelayRecord`/`maxRelayPlaintext`（`:503`/`:521`）与 `sealAndWriteRelayData`（`:532`）的分片尺寸联动；`wire.FrameMeshRelay*` 六个常量（`internal/wire/frame.go:90-128`）只是类型编号，实际线上格式是这套 JSON 信封，改字段必须同步 `internal/mesh/relaytrunk_test.go` 与 `internal/cli/relaytrunk_test.go`。 |
| `internal/mesh/direct.go` 的 `quicConfig()`（`:308`） | 直连与干道两条 QUIC 链路共用它：`quic.Listen`（`direct.go:490`、`relaytrunk.go:2657`）、`quic.Dial`（`direct.go:1100` 直连、`direct.go:1578` 中转会话）；窗口常量 `relayStreamReceiveWindow`/`relayConnectionReceiveWindow`/`relayMaxReceiveWindow`（`:296-306`）决定干道转发是否被流控卡住；`tracingConfig`（`lossstat.go:368`）挂的 QUIC tracer 也在此注入，动了会影响 `mesh/lossstat.go` 的丢包统计与 `mesh/bench.go` 的 `BenchLoss`。 |
| `internal/discovery/snapshot.go` 的快照形状（`MemberSnapshot` `:83` / `MemberView` `:44`） | 锚点每次推送都重建（`relay/mesh.go:539` → `BuildSnapshot` `snapshot.go:124`）；客户端 `verifySnapshot`（`client.go:932`）与 `applySnapshot`（`:993`）按形状解析，`verifySnapshot` 里还会**丢弃**不合法的成员与裁剪 endpoint（`:974`）；`mesh.SnapshotAdapter.KeyFor`（`direct.go:228`）读 `Cert/Endpoints/Delegations`；`mesh.ViewPicker`（`route.go:394`）与 `mesh.RelayCandidates`（`candidates.go:43`）读 `Members`；`cli/status.go:114` 与 `cli/discovery.go:862` 打印视图；`relay/panel.go:164` 的 `/api/mesh` 与 `relay/cockpit_model.go:124` 读同一份数据。改字段形状 = 改线上协议。 |
| `internal/identity/members.go` 的名册结构（`MemberList` `:49` / `Member` `:33`） | 名册文件 `members.json` 的磁盘格式（`SaveMembers` `:433` / `LoadMembers` `:572` / `repairRosterShape` `:610` 修旧形状）；`discovery.Node` 持它并 `SetMembers` 热替换（`internal/discovery/node.go:418`）；`relay.Server` 同时持有同一份（`internal/relay/mesh.go:135`/`:146`），`relayMembersResolver`（`cli/discovery.go:398`）靠它做证书 pin；`discovery.BuildSnapshot`（`snapshot.go:124`）从中取证书、委派与账号 ID；`identity.Delegations` 与 `RevokedCreds` 决定委派证书能否通过客户端复验（`client.go:937-954`）与 `cli/enroll.go:114` 的 `AddDelegation`；`cli/cred.go:214`/`:306` 的 `cred list/revoke` 直接读写它。注意 `Member` **刻意没有 Endpoints 字段**（`members.go:22-32`），`Member.LastSeen` 只在续期时被原样拷回（`:352`），实际在线时间在 `discovery.Node.lastSeenAt`（`internal/discovery/node.go:275`）。 |
| `internal/cli/discovery.go` 的 join 流程（`runJoin` `:491`） | 一次改动同时牵动：凭据入网（`join_cred.go:54`）与邀请入网两条分支（`:551` vs `:577`）、设备身份创建（`join_cred.go:144`）、监听器绑定顺序（`:626`，端口先绑定再 announce）、`discovery.NewClient` 的 `Endpoints`/`NoAnnounce`（`:670`）、`liveViewResolver` 的挂载（`:695`，注释说必须**无条件**挂）、`discovery.json` 的落盘形状（`discoveryConfig` `:324`）、审批等待与撤销后重新等待的时序（`:806`/`:956`）、优雅退场（`gracefulLeave` `:850`）、证书续期协程（`:827`）、relay trunk 加入协程（`:843`），以及 4 个测试 seam（`:69`/`:83`/`:85`/`:1711`）。**这是全仓耦合最紧的一个函数。** |

---

## 9. 已知的耦合点与债务

以下每条都是读代码得到的（`[查证]`），不是推测。

### 9.1 `internal/relay/` 与 `internal/mesh/` 零代码复用（确认）

**确认结论：这两个包之间没有任何 import，零代码复用。** [查证]
`grep -rn "internal/mesh" internal/relay/` 无结果；`grep -rn "internal/relay" internal/mesh/` 只命中
`internal/mesh/relaytrunk.go:26` 的一句**注释**（「The pairing relay (package internal/relay) …」）。

它们是**两个不同的东西**，边界如下：

| | 阶段 1 配对中转（`internal/relay`） | mesh relay trunk（`internal/mesh`） |
|---|---|---|
| 谁在用 | `cmesh send` / `cmesh recv` | `cmesh mesh ping` / `msg` / `send-to` / `run` / `bench` / `status --probe` |
| 配对凭据 | 共享口令 code（房间号） | 设备 ID（成员视图） |
| 传输 | WebSocket over TCP（`internal/relay/relay.go:557`） | QUIC over UDP（`relaytrunk.go:2521` `ServeConn`） |
| 转发方式 | 直接 splice 两条字节流（`internal/relay/relay.go:763`） | 记录级转发 + 端到端封签（`relaytrunk.go:1212`） |
| 密码学 | SPAKE2（`internal/secure`） | SIGMA over X25519（`internal/mesh/session.go:166-183`） |
| 中转看得见明文吗 | 看不见（帧已封） | 看不见（载荷是 E2E 密文，中转只搬记录） |
| 授权 | 知道房间号即可 | 干道判两端都是成员（`relaytrunk.go:354`）+ 配额（`:1035`） |

**债务**：两套转发实现、两套密码学、两套「谁在中间」的语义并存于一个二进制
（`serve-discovery` 同时起两者，`cli/discovery.go:1110` 的 `relay.New` 与 `:1177` 的
`mesh.NewRelayTrunk`）。两者的失败模式、诊断口径、测试夹具都不共享。**不要把它们混为一谈**：
`internal/relay` 是「阶段 1 配对」，`internal/mesh` 是「mesh 干道」。

### 9.2 耦合点清单

| # | 耦合点（file:line） | 一句话 |
|---|---|---|
| 1 | `internal/cli/discovery.go:85`、`:69`、`:83`、`:1711` | 四个包级测试 seam 是**全局可变状态**，生产恒 nil，每个使用者都得自己 save/restore（`cli/discovery_test.go:184`、`:265`、`cli/join_timeout_test.go:144`、`:306`、`cli/relaytrunk_test.go:92`、`:104`、`cli/cred_renew_test.go:84`）。 |
| 2 | `internal/cli/discovery.go:491`（`runJoin`，1494 行文件里的 500 行函数） | join 流程同时承担：凭据/邀请双分支、身份创建、监听器绑定、客户端启动、配置落盘、审批等待、撤销重等待、优雅退场、续期、trunk 加入——一个函数改一处要连带验证十处。 |
| 3 | `internal/cli/discovery.go:376`（`liveViewResolver`）+ `internal/mesh/direct.go:221`（`SnapshotAdapter`） | CLI 侧自造了一个 `mesh.MemberResolver` 适配器，把「discovery 快照」翻译成「mesh 能用的 key 提供者」；`mesh` 包的 resolver 接口（`direct.go:184`）因此被两个不同实现满足。 |
| 4 | `internal/cli/discovery.go:398`（`relayMembersResolver`）+ `internal/mesh/direct.go:191`（`MemberListAdapter`） | 锚点侧又一套适配器，把「热重载的名册」翻译成 resolver；同一个接口两套适配器，两条路径的撤销生效时机不同（快照 vs 名册）。 |
| 5 | `internal/mesh/relaytrunk.go:445`（`SetRelayTrunk`）/ `:453`（`InstalledRelayTrunk`） | 干道是**进程级单例**，通过包级全局变量传递；`cli/discovery.go:1217` 装、`relaytrunk.go:2531` 读。进程内只能有一个干道。 |
| 6 | `internal/mesh/relaytrunk.go:493` vs `:569` | 记录帧**不走 `wire.Frame` 头**，而是长度前缀的 JSON 信封；但 `wire` 包里仍留了六个 `FrameMeshRelay*` 常量与一组 `*Frame` 构造器（`:163`/`:172`/`:798`/`:807`/`:837`），生产路径**一个都不调用**（只有 `AnswerFrame` `:401` 在测试里用到）。这是「设计先行、实现改了路线」留下的死代码。 |
| 7 | `internal/mesh/relaytrunk.go:498`（`relayProtoID = "cmesh-relay/1"`） | 常量声明了协议标识，但**线上记录里根本没有这个字段**（`relayRecord` `:569` 无对应字段），代码里也无人引用。协议版本无处校验。 |
| 8 | `internal/mesh/direct.go:691` | 注释自承：`KindExec`（任意远程命令）**尚未实现**，被代码扫描规则挡着；现在只有内建只读动词（`@info`/`@disk`/`@mem`/`@cat`/`@ls`，`internal/mesh/exec.go:91` 的 `receiveRun`）。`KindExec` 常量（`direct.go:74`）和 `ListenerConfig.ExecDisabled`（`:401`）已就位但无实现。 |
| 9 | `internal/mesh/relaytrunk.go:387` + `:50`（`MaxRelayHops = 1`） | 多跳中转**未实现**：请求跳数超过 1 一律拒绝（`:389`）。`Path.Hops` 字段（`route.go:63`）与 `RelayOpen.Hops`（`:109`）为将来预留。 |
| 10 | `internal/identity/members.go:33`（`Member` 无 `Endpoints`）+ `:36`（`LastSeen`） | `Member.LastSeen` **只被续期路径原样拷回**（`:352`），从不写入真实在线时间；真实在线时间在 `discovery.Node.lastSeenAt`（`internal/discovery/node.go:275`）。名册里那个字段是**永远为零值**的。 |
| 11 | `internal/cli/status.go:204`（`statusReachability`） | 函数体只 `return reachabilityUnknown`，参数 `_ discovery.MemberView` 未用——**快照证明不了可达性**，只有 `--probe` 打到的那一台才有结论（`internal/cli/status.go:166` `probeMember`）。 |
| 12 | `internal/cli/status.go:211`（`snapVersionTime`） | 快照里没有生成时间戳，函数直接返回 `time.Now()`，注释明说「不假装是发现节点给的」。 |
| 13 | `internal/mesh/direct.go:359-361` | `cfg.InsecureSkipVerify = true` + `cfg.VerifyConnection` 精确 pin。注释（`:340-345`）自承这是为了绕开 macOS 系统校验器对自签 Ed25519 证书的拒绝。**这是有意的、被记录在案的取舍**，不是遗漏。 |
| 14 | `internal/cli/node.go`（`Node`，阶段 1 的 peer/mux/p2p/p2pquic 组合） | 与 mesh 主线**完全不相交**（`internal/cli/node.go` 不 import `mesh`/`discovery`/`identity`），走的是 `internal/peer` + `internal/secure` + `internal/mux` + `internal/p2p` + `internal/p2pquic` 另一条栈；`internal/secure`（SPAKE2）只被 `peer` 用，`mesh` 明确拒绝用它（`internal/mesh/session.go:170`）。两套栈共用一个二进制。 |
| 15 | `internal/mesh/relaytrunk.go:492-495`（`relayProtoID` 上方的注释块） | 注释里说「records 走长度前缀 JSON，**不是** `wire.Frame` 头」，与 `internal/wire` 包的存在形成**语义分叉**：wire 包注释（`wire/frame.go:5-9`）说「帧在加密信封内由中转当不透明二进制搬运」，而 mesh 干道根本不用那个信封格式。 |
| 16 | `internal/discovery/node.go:160-163` | `Node.Register` 里**直接写 map** 而不调 `SetRelayEndpoints`，因为后者会二次加锁造成自死锁（注释明说这个 bug 曾挂死整个包）。同类危险代码，注释就是唯一的护栏。 |
| 17 | `internal/cli/discovery.go:107`（`discoveryAllowLocal`） | 出站地址策略的**测试逃生口**，由 `cmeshtest` build tag 控制（`internal/cmeshtest/cmeshtest.go:23`）。发布构建与 CI 不带该 tag，所以生产二进制没有这个洞；但 `internal/cli/join_cred.go:90` 与 `internal/cli/discovery.go:108` 各有一处独立的 `cmeshtest.Enabled` 判断——**两处都要记得**，漏一处就多一个口子。 |
| 18 | `internal/cli/discovery.go:132`（`discoveryAllowPrivate`） | 私有部署开关靠**环境变量**而非 flag（`:133`），因为它要作用于每一个拨号锚点的命令（join/msg/send-to/run/ping），而每个命令各解析各的 flag。 |
| 19 | `internal/mesh/direct.go:1056-1072`（`dialOne` 注释） | quic-go 对 caller-owned `net.PacketConn` **不代为关闭**，所以 `Link` 自己持有 `udp` 并在 `Close` 里释放（`:1155`、`:1346`）。socket 生命周期靠注释约定，编译器不管。 |
| 20 | `internal/mesh/session.go:53`（`Session` 接口的 `Close` 是半关闭） | 语义（FIN 而非全关）是**接口契约里的一段散文**（`:36-48`），不是类型能表达的；`relaySession.Close`（`relaytrunk.go:1561`）、`quicSession.Close`（`internal/mesh/session.go:118`）等每个实现都得自己记住。 |
| 21 | `internal/mesh/errors.go:85`（`refusalMarker`） | 「对端拒绝」这个判定靠一个私有 marker 类型 + `classifyDialError` 统一归类（`errors.go:118`），而不是在抛出点直接包 `ErrRefused`。分类只在**一个**地方发生，是刻意设计，但新增错误路径时容易漏。 |
| 22 | `internal/relay/mesh.go:365`（`handleMeshMembers`）+ `internal/relay/relay.go:292`（`Handler`） | 发现路由**只在 `s.disco != nil` 时才挂载**（`internal/relay/relay.go:305`）；没开 discovery 时 `/mesh/*` 静默 404。`/panel` 与 `/api/mesh` 也被同一个 `if` 门控（`internal/relay/relay.go:308`）。 |
| 23 | `internal/relay/panel.go:164`（`handleAPIMesh`）+ `internal/relay/cockpit_model.go:124` | 面板的**同一份数据**要同时满足两个消费者：轻量面板（`PanelMember`）与驾驶舱原型（`CockpitMember`）。`PanelMember` 字段是两者字段集的**超集**（`panel.go:37-40` 注释明说），原型改名会被直接带进线上载荷。 |

### 9.3 结构性观察（非缺陷，是「读代码时会绊到的地方」）

- **`internal/mesh` 是唯一同时被 CLI 直连依赖、又依赖 discovery 的包**。它从
  `discovery.MemberSnapshot` 里取三样东西：证书（pin 用）、`Endpoints`（直连）、
  `RelayEndpoints`（中转候选，`route.go:424`）。任何快照形状的改动都会打到传输层。
- **「中继只兜底」这条产品规则实现在 `ViewPicker.Pick` 一处**（`route.go:404-405`
  把直连放在 `out[0]`）。`tryPaths`（`:139`）决定什么情况下换路：只有
  `ErrAuth`/`ErrRefused` 这类**判决**立即停止，其余换下一条（`:159-163`）。
- **错误分类是路径切换的唯一依据**（`errors.go:16-27` 注释写死了这个契约）。
  新增一个错误类型时，如果不落到 `dialClasses`（`errors.go:70-76`）里，
  它会被当成「未知失败」→ 换路重试，把一次判决变成 N 次尝试。
- **`MemberSnapshot` 是全量而非增量**（`snapshot.go:69-76`），版本号防乱序；
  `applySnapshot` 要求版本单调（`client.go:996`）。

---

## 10. 附：本文的核对方法

```bash
# 1. 包依赖图（§5）
go list -f '{{.ImportPath}}: {{join .Imports "\n  "}}' ./internal/...

# 2. 环检查：项目内包之间的边（§5.3）
go list -deps -f '{{.ImportPath}} {{join .Imports " "}}' ./... | grep muzimu217

# 3. 两个「relay」零复用（§9.1）
grep -rn "internal/mesh"  internal/relay/
grep -rn "internal/relay" internal/mesh/

# 4. 测试 seam 的使用者（§1.3）
grep -rn "joinLifetimeHook\|joinResolverHook\|discoveryClientTransportHook\|certRenewIntervalHook" --include="*.go" .

# 5. quicConfig 的全部调用点（§8）
grep -rn "quicConfig()" --include="*.go" .
```

> **注意**：本仓库工作树当前有未提交改动（撰写时 `git status` 显示 16 个已跟踪文件被修改）。
> 上述命令应在提交后的干净树上重跑一次以确认 file:line 未漂移。
