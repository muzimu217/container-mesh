# M4 验收清单：外部验收与真实环境复测

> **位置**：三层文档的第三层（具体怎么验）。上层：[`../REQUIREMENTS.md`](../REQUIREMENTS.md)（要什么）、
> [`../DESIGN.md`](../DESIGN.md)（用什么结构）、[`../PLAN.md`](../PLAN.md)（做到哪一步）。
> 本文的读者是**下一个外部测试员**——你不需要读过本仓库的历史，只需要照着 §4 抄命令。
>
> **状态**：本文是**验收方法**，不是验收结果。文中所有 `[实测]` 数字都注明出处（轮次/决策号）；
> 未标 `[实测]` 的期望值一律是**判据**，不是已经发生的事实。
>
> **本文回答什么**：M4 的每个能力，**谁能验**、**怎么验**、**期望什么**、**卡住了该找谁**。
>
> **版本**：V1（2026-11-19）。对应 `develop@96fdef8`。

---

## 0. 一句话摘要

M4 的外部四线（oh-no / doubao / kuku / netns）反复卡住的**不是产品行为错**，而是**结构性够不到**：
某些验收项**必须在锚点本机跑**（名册文件在锚点上），某些需要**值守先签发凭证**，
某些被**测试拓扑伪影**（hairpin、私网地址被出口策略拦）挡住。

因此本文的三条组织原则：

1. **能力矩阵**按「谁能做」分列，不按功能分组——外部代理拿不到的东西，不要写进它的任务单。
2. **「结构性够不到」清单**（§3）单独成章，写成给下一个测试员的说明书。
3. **每条命令都带退出码和期望输出片段**（§5），让人能直接照抄，不靠猜。

---

## 1. 角色与术语

| 术语 | 指谁 | 能碰什么 |
|---|---|---|
| **锚点 / anchor** | 跑 `cmesh serve-discovery` 的那台机器 | 名册 `members.json`、账号公钥、凭证签发、面板令牌、trunk 开关 |
| **成员设备** | 跑 `cmesh join` 的机器 | 自己的 `device.json` / `discovery.json` / `inbox/`；**没有名册** |
| **值守** | 协调者（主代理；六道门见 `../CONVENTIONS.md` §6） | 签发凭证、放行 push、给外部代理派单 |
| **外部代理** | oh-no / doubao / kuku / netns / muse 四线测试员 | 在自己的环境里跑命令、回报证据 |
| **网主** | 用户 | 授权测试主机、授权 reboot、批准发布动作 |

**测试主机铁律**（`docs/decisions.md` D78 第 1 条）：**测试主机必须由网主指定**。
`175.24.139.148`（blackevil.cn）是当前唯一授权的容器网格测试主机。
不要从 `~/.ssh/config` 的别名里随便挑一台——把别的项目的生产机当测试机用，已经发生过一次，
代价是几小时和一轮错误结论。

---

## 2. 验收矩阵

**列的含义**

- **验证方式**：怎么判定通过。
- **需要的权限/角色**：这件事物理上谁才能做。
- **谁能做**：外部代理 / 值守 / 需网主授权。

**行的含义**：一个可独立判定的验收项。**一个格子 PASS 不代表整行 PASS**——每格单独记。

### 2.1 组网与身份

| # | 能力 | 验证方式 | 需要的权限/角色 | 谁能做 |
|---|---|---|---|---|
| A1 | `init` 建设备身份 | `cmesh init --state DIR` 后 `device.json` 存在、退出码 0 | 本机写权限 | 外部代理 |
| A2 | `account create` 建账号 | 同上，产出 `account.json` | 本机写权限 | 外部代理（**但账号持有方是网主侧角色**） |
| A3 | `account show` 取账号公钥 | stdout 出现 `pubkey.b64 ` / `pubkey.hex ` 两行 | 账号口令 | 值守 / 网主 |
| A4 | `account device-add` 把本机签进账号 | 产出 `<deviceID>.cert.json`，退出码 0 | 账号 + 设备口令 | 外部代理（锚点自己） |
| A5 | `invite` 生成入网串 | `cmesh invite --disc host:port` 打印 `cmesh://join?…` | 无 | 外部代理 |
| A6 | **邀请式 join**（人工审批路径） | `join <invite>` → 打印 `PUBKEY` → 所有者 `account authorize --pub` → 证书落 `certs/` → joiner 打印 `approved: this device is now a member` | 账号所有者（批准那一步） | 外部代理 + 值守配合 |
| A7 | **凭证式 join**（一条命令入网） | `cmesh join --cred <串>` 退出码 0，stderr 含 `Enrolled with a team credential` | **一份锚点签发的凭证** | **外部代理（但凭证必须先由值守签发）** |
| A8 | 凭证额度生效 | 用 `--max-devices 2` 的凭证，第 3 台入网被拒，文案含 `credential quota exhausted` | 凭证签发权 | 值守 + 外部代理 |
| A9 | 凭证过期生效 | 用 `--ttl 1m` 的凭证，过期后入网被拒 | 凭证签发权 | 值守 |
| A10 | 成员自续期 | 长跑 `join --cred`，观察 stderr 的 `certificate renewed` | 短 TTL 凭证 | 值守（用 `certRenewIntervalHook` 压缩） |
| A11 | 成员退出（LEAVE） | 对 join 进程发 SIGTERM，锚点视图 ≤3s 变 offline（预算 P-5b） | 本机进程控制 | 外部代理 |

### 2.2 状态与连通性

| # | 能力 | 验证方式 | 需要的权限/角色 | 谁能做 |
|---|---|---|---|---|
| B1 | `status`（成员视图） | `cmesh status` 退出码 0，列出成员、online、endpoints、last_seen | 成员设备 | 外部代理 |
| B2 | `status --probe <id>` | `reachability=direct` 或 `relay`；**无 probe 时必须是 `unknown`，不得伪造** | 成员设备 | 外部代理 |
| B3 | `mesh ping`（直连 RTT） | `--- <id>: N/N pings, lost 0 (0.0%), p50 …, link=direct` | 成员设备 | 外部代理 |
| B4 | `mesh ping`（中转） | 输出含 **`link=relay via=<锚点deviceID>`** | 成员设备 + 干道已接通 | 外部代理 |
| B5 | `msg`（按名消息） | 发送端 stderr `delivered 1 message(s) to <id>`；接收端 join 进程打印 `📩 from …` | 成员设备（两端都起 join） | 外部代理 |
| B6 | `send-to`（按名文件） | 发送端 `receiver verified` + sha256；接收端 `📬 from …` 且本地 sha256 逐字节相等 | 成员设备 | 外部代理 |
| B7 | `run`（内置只读动词） | `cmesh run <id> @info` 输出 `hostname/os/version/go` 五行 | 成员设备 | 外部代理 |
| B8 | `run @cat` 大输出 | 4095 / 4096 / 4097 / 65536 / 1048576 B 全部字节精确 | 成员设备 | 外部代理 |
| B9 | `bench` 双向吞吐 | 输出 `direction=both size_each=… sent=… received=…` 且 sent==received | 成员设备 | 外部代理 |
| B10 | `bench` 字节完整性 | 4096/1MiB/8MiB/32MiB 与**非对齐尺寸**（4095/4097/65537/1048577/33554431）双向 sent==received | 成员设备 | 外部代理 |

### 2.3 撤销、拆流、配额

| # | 能力 | 验证方式 | 需要的权限/角色 | 谁能做 |
|---|---|---|---|---|
| C1 | **撤销拆流（直连）** | 撤销后既有会话在一个推送往返内被销毁 | **锚点**（名册在锚点） | 值守（锚点侧） |
| C2 | **撤销拆流（中转）** | 中转活流被强制拆掉（abort，非 FIN） | 锚点 + 干道 | 值守 |
| C3 | 撤销后新握手被拒 | 客户端报「已被移出网络 / not a member」 | 成员设备 | 外部代理 |
| C4 | `cred revoke`（凭证吊销，级联踢出） | 输出 `N device(s) left the network` 并列出 ID；名册里该凭证的成员消失 | **锚点**（`members.json` 在锚点） | 值守 |
| C5 | `cred list`（含吊销列表） | 末尾出现 `revoked credentials in …` 一节 | **锚点** | 值守 |
| C6 | 凭证预吊销（未入网也能吊销） | 全新锚点（无 `members.json`）上 `cred revoke --cred-id X` 退出码 0 | **锚点** | 值守 |
| C7 | 吊销防复活 | 被吊销的凭证再次入网被拒，文案 `credential X has been revoked` | 锚点 + 外部代理 | 值守 + 外部代理 |
| C8 | 中转配额（流数 / 速率） | `--relay-max-streams` / `--relay-max-bytes-per-sec` 生效；超限报 `quota exceeded` | 锚点（开关在锚点） | 值守 |

### 2.4 面板

| # | 能力 | 验证方式 | 需要的权限/角色 | 谁能做 |
|---|---|---|---|---|
| D1 | 面板页面可达 | `GET /panel` → **308** → `GET /panel/` → 200；308 带 `Cache-Control: no-store` | 锚点 + 面板令牌 | 外部代理（令牌由值守给） |
| D2 | 面板令牌三态 | 无令牌 → 401；`?token=` 查询串 → **401**（只认 header）；`X-Panel-Token: <tok>` → 200 | 同上 | 外部代理 |
| D3 | 面板无假数据 | 页面/产物里 `strings`/`grep` 命中 0 个演示设备名与假遥测字段 | 锚点二进制 | 外部代理 |
| D4 | 面板名册真实性 | 3D 场景设备数 == `GET /api/mesh` 的 `members` 长度 | 同上 | 外部代理 |
| D5 | 离线成员保留 | 成员下线后仍出现在名册里，带 `last_seen` | 同上 | 外部代理 |
| D6 | 聚合不把缺失当零 | 某成员遥测缺失时，聚合卡显示 `未采集`，**不是** `0.0 MB/s` | 同上 | 外部代理 |

### 2.5 中转（干道）四项

前置：D76 裁定**默认同端口双 ALPN**（`--relay-port 0`），网主不必再开端口。

| # | 能力 | 验证方式 | 需要的权限/角色 | 谁能做 |
|---|---|---|---|---|
| E1 | 锚点开干道 | `serve-discovery --relay-trunk` 启动横幅出现 `relay trunk: ON`；**不加**该开关时必须是 `relay trunk: off` | 锚点 | 值守 |
| E2 | 纯出站成员加入干道 | `join --relay-trunk <锚点可达地址> --relay-trunk-id <锚点deviceID>` | 锚点地址 + 锚点 deviceID | 外部代理（参数由值守给） |
| E3 | 中转四项 | `mesh ping` / `msg` / `run @info` / `send-to` **全部走中转**且成功 | 同上 | 外部代理 |
| E4 | 中转端到端加密 | tcpdump 抓包，明文出现 **0 次**；正向对照（不加密时能抓到） | 网卡抓包权限 | **需网主授权**（在授权主机上抓包） |
| E5 | 中转大文件 | 3 MiB 经中转 `receiver verified`，双端 sha256 逐字节相等 | 同 E2 | 外部代理 |
| E6 | 撤销对中转活流生效 | 撤销后中转流被拆；请求方看到可操作的拒绝理由 | 锚点 | 值守 |
| E7 | 直连优先未回归 | 中转可用时，`mesh ping` 到**有端点的**成员仍报 `link=direct` | 成员设备 | 外部代理 |

### 2.6 安装、发布、systemd

| # | 能力 | 验证方式 | 需要的权限/角色 | 谁能做 |
|---|---|---|---|---|
| F1 | 安装器下载 + 校验 | `cmesh-install` / `install.sh` 下载 + sha256 校验 + 落盘，退出码 0 | 目标机 root | 外部代理（需 root） |
| F2 | `cmesh verify` 本地校验 | 匹配 → rc=0；篡改 → **rc=1** 且拒绝执行 | 无 | 外部代理 |
| F3 | 版本目录权限 | 安装后 `versions/<v>/` 是 **0755**（不是 0700），非特权用户能执行 | 目标机 | 外部代理 |
| F4 | systemd 启动 | `systemctl enable --now` → active；`journalctl` 有日志 | 目标机 root | 外部代理（需 root） |
| F5 | systemd 优雅停止 | `kill -TERM` → **`ExecMainStatus=0`**（不是 143）；端口立即可重绑 | 同上 | 外部代理 |
| F6 | systemd 崩溃自愈 | `kill -9` → ≤5s 自动拉起 | 同上 | 外部代理 |
| F7 | 卸载语义 | `--uninstall` 保留状态；`--purge` 删状态 | 同上 | 外部代理 |
| F8 | **reboot 恢复** | 重启后服务自动起来、成员仍在网 | **需要重启生产服务器** | **需网主授权** |
| F9 | Release 资产完整性 | `SHA256SUMS` 行数 == 实际资产数，**无悬空行**；每个资产 URL 200 | 网络 | 外部代理（`gh release view` + 逐 URL） |
| F10 | 真实 WAN 链路 | 公网 `mesh ping` 100/100、0 丢 | **需网主指定主机 + 开放入站 UDP** | 需网主授权 |

---

## 3. 「结构性够不到」清单（给下一个测试员的说明书）

> 这一节是本文的核心。前面几轮外部复测，大部分时间**不是花在测产品上，而是花在撞一堵结构性的墙**。
> 把这堵墙写清楚，下一轮就能省掉那几轮。

### 3.1 墙一：`cred list` / `cred revoke` 读的是锚点本机的名册文件

**事实**（代码核实）：

- `cred list` 解析名册路径 `identity.IdentityPath(dir, identity.MembersFileName)`
  （`internal/cli/cred.go:145-151`），即 `<state>/members.json`。
- 名册**只有锚点有**。成员设备的 state 目录里只有 `device.json` + `discovery.json`。
- 程序**能分辨这两种目录**（`internal/cli/cred.go:124-143`）：目录里有 `discovery.json`
  或 `device.json` → 判定为「成员设备」；否则判定为「锚点」。
- 因此在**成员设备**上跑 `cred list` / `cred revoke`，程序明确拒绝并给出指路：

  ```
  cmesh: <dir> has no roster: the member list belongs to the discovery node, and this directory is a member's own state
  (device.json + discovery.json). Run the command on the anchor, where members.json lives.
  ```
  退出码 **1**。（[实测]：用本地构建的二进制在伪造的成员目录上跑出来的原文。）

- 在**全新锚点**（`members.json` 还不存在）上跑，程序**不报错**，而是当成空名册：
  `cred list` → `no roster yet in <dir> (this anchor has never had a member; nothing to list)`，退出码 **0**；
  `cred revoke` 则**新建**名册并写入吊销记录（这是 Round 69 修的功能缺口，见 §7）。

**结论**：**`cred list` / `cred revoke` 只能在锚点上跑。** 成员设备上跑必然 rc=1，
这不是缺陷，是设计。

**值守该怎么配合**（三选一，按成本从低到高）：

| 方式 | 值守做什么 | 测试员做什么 | 适用 |
|---|---|---|---|
| **① 值守代跑并贴输出** | 在锚点上跑 `cred list --state <锚点state>`、`cred revoke --state <锚点state> --cred-id X`，把 **stderr 全文 + 退出码**贴进回报 | 只需验证「成员是否真的被踢出」（用 `status` / 面板看） | 绝大多数情况（推荐） |
| **② 值守提供锚点的只读视图** | 把锚点的 `members.json` 摘要（成员数、各凭证 members 计数、吊销列表）作为**只读回执**发出来 | 对比自己的观察 | 测试员要断言「名册里发生了什么」时 |
| **③ 给测试员锚点侧的只读 shell** | 在授权主机上提供一个只能读 `<state>/members.json` 的入口 | 自己跑 `cred list` | 需要反复轮询名册时 |

**不要**做的事：不要因为成员设备上 `cred list` rc=1 就报缺陷——那是**结构性够不到**，不是产品错。
也不要在成员设备上「造一个 `members.json` 去试试」——那会让本地空名册覆盖锚点的真实名册。

### 3.2 墙二：没有锚点签发的凭证，就入不了网

**事实**：

- `join --cred` 的流程（`internal/cli/join_cred.go`）：解码凭证 → 用凭证里的**委派私钥**在本机签自己的证书
  → `POST <锚点>/mesh/enroll` 准入 → 然后才 announce。
- 凭证由**账号持有方**在锚点侧签发（`cmesh cred create --disc <锚点>`，
  `internal/cli/cred.go:44-103`），**凭证是秘密**（一份字符串 = 入网权限）。
- 锚点侧要挂载 `/mesh/enroll` 端点，**必须带 `--account-pub`**（`internal/cli/enroll.go:51`：
  没有账号公钥时端点直接回答 `this node cannot verify credentials: start it with --account-pub`）。

**已发生的事**：doubao 连续**五轮**卡在同一道门上——「缺锚点签发的凭证」。
第六轮由值守直接签发凭证才解开（`docs/ROUND-LOG.md` Round 69）。

**值守该怎么配合**：

- **签发凭证**：`cmesh cred create --state <锚点state> --account-passphrase <口令> --disc <锚点可达地址> --ttl 24h --max-devices 10`
  - 凭证串打在 **stdout**（叙述打在 stderr）——取值要 `2>/dev/null` **不要**丢掉 stdout。
  - 同一条输出里还有 `credential id: <ID>`（stderr），**这个 ID 是吊销的钥匙**，要一起给测试员。
- **给出锚点的 `--account-pub`**（base64 或 hex 都收，`cmesh account show` 的 `pubkey.b64` 行）。
- **给出锚点的可达地址**（写进凭证的 `--disc`，测试员不需要自己知道）。

**测试员自己能不能造凭证？** 不能。`cred create` 需要账号私钥（`account.json`），
那是**网主/账号持有方**的东西。测试员拿到的是**别人签好的凭证**，不是签发权。

### 3.3 墙三：`show-pub` 是设备公钥，`account show` 才是账号公钥

**事实**（Round 38 记录的操作坑，oh-no 实踩）：

- `cmesh show-pub` 打印**设备**公钥。
- `cmesh cred create` 的凭证要能被**账号**验签 → 要的是**账号**公钥。
- `cmesh account show` 的 `pubkey.b64` / `pubkey.hex` 两行打在 **stdout**（`internal/cli/mesh.go:370-371`）。

**取值写法**（照抄）：

```bash
ACC=$(cmesh account show --state "$ANCHOR_STATE" --account-passphrase "$PASS" 2>/dev/null \
      | sed -n 's/^pubkey.b64 //p')
```

> ⚠️ `--account-passphrase` 与 `--passphrase` 两种拼法都收（`internal/cli/mesh.go:339-340`），
> 但**其它命令不收别名**：`cred create` 只收 `--account-passphrase`，
> `join` / `status` / `msg` 只收 `--device-passphrase`，`init` 只收 `--passphrase`。
> 写错拼法会得到 `flag provided but not defined`。

### 3.4 墙四：锚点自己没有 `discovery.json`，所以锚点不能主动发起成员操作

**事实**：oh-no 在 B-4 候选里发现——锚点 `serve-discovery` 的 state 里**没有** `discovery.json`，
所以锚点自己跑 `mesh ping` / `msg` 时报 `config not found`（`internal/cli/meshcmd.go:67-71`
读的就是 `<state>/discovery.json`）。

成员 → 锚点、成员 ↔ 成员都正常。

**结论**：**锚点是「被连方」，不是「发起方」**。验收矩阵里凡是「锚点主动 ping 某成员」的项，
在当前形态下**做不了**，不要报缺陷，也不要试图在锚点 state 里手造 `discovery.json`。

**值守该怎么配合**：要验「锚点 → 成员」方向，用**第三台成员**发起（这在拓扑上是等价的，
且 netns 线的 D75 判据正是这么定的：**私网端点公告 + 无入站映射 = 预期不可达**，
`docs/decisions.md` D75）。

### 3.5 墙五：出口策略会挡住私网/回环的发现源（除非显式开开关）

**事实**（Round 56 / D78 相关）：

- 生产默认**拒绝**解析到回环/私网/链路本地/保留段的发现地址（三条出站路径都受约束：
  members WS、register HTTP、enroll HTTP）。
- 私网部署的解法是环境变量 **`CMESH_ALLOW_PRIVATE_DISCOVERY=1`**
  （`internal/cli/discovery.go:132-133`）。放宽是**刻意收窄**的：私网/回环/链路本地放行，
  **保留段与文档段仍然拒绝**，**按名字的 `localhost` 仍然拒绝**。
- 它是**环境变量而不是 flag**，因为它必须对**每一个拨号锚点的命令**生效
  （join / msg / send-to / run / ping），而每个命令各自解析自己的 flag。

**⚠️ 但这个开关对「邀请式 join」不生效**（[实测]，详见 §7.5）：

```
$ export CMESH_ALLOW_PRIVATE_DISCOVERY=1
$ cmesh join --listen 127.0.0.1:0 --device-passphrase passw0rd --state m --timeout 5s \
      "cmesh://join?acct=…&disc=127.0.0.1%3A64866&k=…"
cmesh: bad invite: discovery: malformed endpoint: "127.0.0.1:64866"
```

**原因**（代码核实）：`discovery.DecodeInvite` 用的是**不可切换的**那个校验函数
（`internal/discovery/member.go:400` 调 `normalizeBaseURL(a)`，
而切换版是 `normalizeBaseURLWith(a, privateAllowed)`，`internal/discovery/client.go:1032-1039`）。
所以**邀请串里的私网/回环发现地址一律被拒**，与那个环境变量无关。

**⇒ 私网 / netns / 回环拓扑一律走凭证式 join**（凭证路径走
`NormalizeDiscoveryURLWith(addr, discoveryAllowPrivate())`，`internal/cli/join_cred.go:101`，
**是切换过的**）。值守在派私网拓扑的任务单时**必须给凭证，不要给 invite**。

**测试员自己怎么判断是不是被策略挡了**：报错长这样（原文，来自 `internal/discovery` 的出口策略）：

```
egress policy: 127.0.0.1:39711 resolved to 127.0.0.1, which is not a public address
```

注意「解析到」三个字——策略判的是**解析后**的地址，不是字面 IP。
而 invite 路径的报错长得**不一样**（`malformed endpoint`），别把两者混为一谈。


### 3.6 墙六：测试主机必须由网主指定

见 §1 的 D78 第 1 条。**值守该配合的是**：派单里写清「本次可用的主机是 X（IP / SSH 别名）」，
不要让测试员自己找。

---

## 4. 易踩的坑清单

> 这一节每一条都对应**已经真实发生过的**一次失败，不是假想。

### 4.1 flag 顺序：Go 的 `flag` 在第一个位置参数处停止

**机制**（[实测]：用最小复现程序验证）：

```
go run m.go DEV --text hi   → text=""   NArg=3  args=[DEV --text hi]   ← flag 被吞成位置参数
go run m.go --text hi DEV   → text="hi" NArg=1  args=[DEV]             ← 正确
```

**产品的写法因此是「deviceID 放最后」**：

| 命令 | 正确写法 | 错误写法 | 错误时的退出码 | [实测] 错误写法的原文 |
|---|---|---|---|---|
| `join`（**邀请式**） | `cmesh join --listen 0.0.0.0:7000 "<invite>"` | `cmesh join "<invite>" --listen 0.0.0.0:7000` | **2** | `usage: cmesh join [flags] <invite-string> \| cmesh join --cred <credential> (the invite goes last: flag parsing stops at the first positional argument)` |
| `msg` | `cmesh msg --text "hi" <deviceID>` | `cmesh msg <deviceID> --text "hi"` | **2** | `usage: cmesh msg [flags] <deviceID> (device ID goes last)` |
| `send-to` | `cmesh send-to --file PATH <deviceID>` | `cmesh send-to <deviceID> --file PATH` | **2** | `usage: cmesh send-to [flags] --file PATH <deviceID> (the device ID goes last)` |
| `mesh ping` | `cmesh mesh ping --count 30 <deviceID>` | `cmesh mesh ping <deviceID> --count 30` | **2** | usage 错误 |
| `run` | `cmesh run <deviceID> @info`（动词在 deviceID 后） | `cmesh run @info <deviceID>` | **2** | `"@info" is not a valid device ID` |

> ⚠️ **`join --cred` 是例外**：凭证式的 `join` **没有位置参数**
> （`--cred` 与 invite 串互斥，`internal/cli/discovery.go:440-445`），
> 所以 **`join --cred "$CRED" --listen 0.0.0.0:7000` 是正确的**——
> [实测]：在真实锚点上跑完整流程（签发凭证 → `join --cred` → 入网成功 → `listening for direct links on …`），
> **rc 无误、正常入网**。把「`--cred` 后跟 `--listen` 被当位置参数」当成通用规则会误导测试员。
>
> oh-no 实踩过的两处不一致（`docs/ROUND-LOG.md` Round 38 的操作坑登记）：
> `msg -text` 在 deviceID **之前**、而 `send-to` 在**之后**——**两者都要求 deviceID 在最后**。
> `send-to` 的帮助文本已经写明「the device ID goes last」（`internal/cli/meshcmd.go:355`）。

**一个额外发现的缺口**（见 §7.5）：**邀请式的 `join` 在私网拓扑下根本走不通**——
`CMESH_ALLOW_PRIVATE_DISCOVERY=1` 对 invite 路径**不生效**。私网 / netns / 回环拓扑一律用凭证式 join。


### 4.2 环境变量

| 变量 | 作用 | 谁需要它 |
|---|---|---|
| `CMESH_PASSPHRASE` | 口令回退（优先级：flag > 本变量 > 交互提示，`internal/cli/mesh.go:44-57`）。**服务无 tty 时空口令会 EOF 退出** | 容器 / systemd / CI |
| `CMESH_ALLOW_PRIVATE_DISCOVERY=1` | 放行私网/回环/链路本地的**发现源**（§3.5） | 私网 / netns / loopback 拓扑 |

**口令长度**：设备与账号口令**至少 8 字符**（`cmesh init` 会拒更短的，实测 `passphrase must be at least 8 characters`）。

### 4.3 回环与 netns：无 `CAP_NET_ADMIN` 就做不了真 NAT

- 本产品的定位就是**没有 `CAP_NET_ADMIN`、没有 `/dev/net/tun`、没有入站端口**的环境
  （`docs/REQUIREMENTS.md` §1.2 与 `docs/research/userspace-netstack.md`）。
- 所以**在 netns 里搭真 NAT（veth/iptables MASQUERADE）需要额外权限**。
  没有 `CAP_NET_ADMIN` 的沙箱里只能做**回环等价形态**——**这时必须如实标注拓扑**，
  不要把回环数字写成 NAT 数字（kuku 在 Round 51/55 就是这么做的，结论可用，标注也清楚）。
- **回环形态下仍要验证的**：命令契约、退出码、输出格式、字节完整性、面板黑盒。
- **回环形态下验证不了的**（必须换环境或标「未验」）：NAT 方向性、hairpin、大文件公网吞吐。

### 4.4 Docker 拓扑：锚点**必须宣告私网地址**

**这是本轮最贵的一个坑**（`docs/decisions.md` D78 第 2 条）。

决定性 A/B（同一文件、同一路径、连续四次 3 MiB）：

| 锚点宣告地址 | 结果 |
|---|---|
| **私网** `10.0.0.14:18090`（容器直连，无 hairpin） | **4/4 成功，每次 0–1 秒** |
| **公网** `175.24.139.148:18090`（容器必须绕 hairpin） | 1 次挂（61s 超时），其余 23–79s |

**机制**：宣告公网地址时，容器的每个包都要出 eth0、过云 NAT、再回来——**hairpin 是一次真实的公网往返**，
而那条路径实测**在 ~7–9 Mbps 就开始丢 bulk UDP**（`docs/ROUND-LOG.md` Round 62）。成员流（小包、低速率）能活，
3 MiB 的批量传输不能。

**曾经被这个伪影带偏的结论**（记录下来避免重犯）：
「≥256 KiB 的文件经中转会挂死」「Reno 拥塞窗口下限」「客户端 UDP 校验和卸载故障」
——三个解释**全被推翻**。真因是 hairpin。

**结论**：「中转大文件会挂」**不是缺陷**。真机重测：44 B / 64 K / 256 K / 1 MiB 全部 `receiver verified`，
3 MiB 22s `receiver verified`、sha256 逐字节匹配。

### 4.5 容器要 `--platform linux/amd64`

容器里跑**发布件**时必须显式指定平台，否则会拉到 arm64 镜像或在多架构环境下行为不一致：

```bash
docker run --platform linux/amd64 ...
```

> 这条是**发布件在容器里复现**的前提：发布资产是按 `<os>-<arch>` 命名的
> （`cmesh-v1.0.0-rc2-linux-amd64`），装错架构的资产会在运行时才炸。
> 装好的二进制用 `cmesh version` 确认是 `linux/amd64` 那一版。

### 4.6 拉源码：`git fetch` 失败改用 codeload tarball

外部代理用 `git fetch` 拉仓库经常失败（GitHub 443 间歇超时）。**改用 codeload 的 tarball**：

```bash
# 不用 git fetch（443 间歇超时）
curl -fsSL -o cm.tar.gz \
  "https://codeload.github.com/muzimu217/container-mesh/tar.gz/refs/heads/develop"
mkdir -p src && tar -xzf cm.tar.gz -C src --strip-components=1
```

拉下来后**必须核对 SHA**（`.team/certification.md` 里有当轮 tip），
并把 `git rev-parse HEAD`（或 tarball 的目录名）写进回报。

> 只跑「命令契约」类验收时，甚至**不需要源码**——只需要一个能跑的二进制。
> 源码只在需要读代码定位时才必要。

### 4.7 退出码不是 0 就是 1 的地方

| 退出码 | 含义 |
|---|---|
| **0** | 成功 |
| **1** | 运行时失败（网络不可达、不是成员、被撤销、校验不通过……） |
| **2** | **用法错误**（flag 拼错、缺参数、deviceID 不合法、参数越界） |

**用法错误与运行时失败的区分很重要**：rc=2 说明**命令写错了**，rc=1 说明**环境或状态不对**。
外部回报里把两者混在一起会让协调者误判成产品缺陷。

### 4.8 参数边界（会被拒，别当缺陷）

- `mesh ping --count` 必须在 **1..1000**（外 → rc=2）
- `mesh ping --interval` 最小 **100ms**（外 → rc=2）
- `mesh ping --timeout` 默认**按 count × interval 动态算**（15s 发现 + 采样窗口 + 每次 1s 回复余量），
  不要再用固定的 20s 去卡 `--count 1000`（那是旧版的口径，已被 oh-no 报出并修掉）。
- `cred create --max-devices` 必须为正。
- `join --timeout` 不得为负（**0 = 永远等**）。它**只约束等审批那一段**，审批落地即解除；
  成员进程是常驻的（Round 68 修掉的「10 分钟后自动退网」缺陷）。

### 4.9 别把「测量口径」当产品缺陷

两个已定案的判读口径：

| 现象 | 正解 | 出处 |
|---|---|---|
| 成员 kill -9 后，锚点日志里看不到下线记录 | **锚点默认日志级别是 Warn**，快速离线路径**根本不打日志**（那是 Sweep 路径，15s 一次）。**判据一律用面板的 `online` 计数** | Round 56 |
| 成员 RSS 读数 75–85 MB | 那是 **GC 之前的相位**（默认 GOGC 约 120s 才 GC 一次）。**判据用 post-GC 稳态**，或直接采 gctrace。实测 post-GC **12.0 MB ≤ 15 MB 目标 → 达标** | `docs/decisions.md` D74 |

### 4.10 公网上测吞吐要给足超时

R62 的一次自伤：4 MB / 16 MB 全报「FAIL」，实际是**自己的 250 秒超时**——单独跑 4 MB 只要 **6.3 秒**。
**小文件在公网上不慢，大文件慢是字节多，不是握手慢**。测吞吐必须给足超时，
否则会把超时误判成缺陷。

### 4.11 公网 UDP 的物理天花板

真实公网 UDP 在 **~7–9 Mbps** 开始丢包（裸 UDP 探针测的，与本仓代码无关）。
**P-3 的 200 MiB/s 在公网口径下不可能达到**（那需要 1.6 Gbps）。
中转路径的吞吐按「实际带宽可达」标注，不按 P-3 卡（`docs/decisions.md` D73）。

---

## 5. 可复制的命令模板

> **通用约定**
>
> - 变量先设好，后面直接抄。
> - 退出码用 `; echo "rc=$?"` 取。
> - 期望输出只列**必须出现的关键片段**（用 `grep -F` 就能判），不是完整输出。
> - 所有命令**不写死主机名**——用变量。

```bash
# ── 角色与地址 ────────────────────────────────────────────────
export ANCHOR_STATE=/var/lib/cmesh          # 锚点的 state（名册在这里）
export ANCHOR_ADDR=203.0.113.10:18090       # 锚点可达地址（host:port）
export ANCHOR_PUB=""                        # 账号公钥（值守给，base64 或 hex 都收）
export ANCHOR_ID=""                         # 锚点的 deviceID（中转 join 需要）
export PASS='<至少 8 字符的口令>'
export MY_STATE=/tmp/member-state           # 成员设备自己的 state
export CRED=""                              # 锚点签发的凭证（值守给）
export CRED_ID=""                           # 凭证 ID（吊销用）
export PEER_ID=""                           # 对端 deviceID

# 私网 / 回环 / netns 拓扑才需要（每一条拨号锚点的命令都要带）
# export CMESH_ALLOW_PRIVATE_DISCOVERY=1

# 容器 / systemd / CI 才需要（否则会 EOF）
# export CMESH_PASSPHRASE="$PASS"
```

### T1. 锚点上线（值守在锚点跑）

```bash
cmesh serve-discovery \
  --state "$ANCHOR_STATE" \
  --addr :18090 \
  --account-pub "$ANCHOR_PUB" \
  --announce "$ANCHOR_ADDR" \
  --relay-trunk \
  --panel \
  > /tmp/anchor.log 2>&1 &
```

- **期望退出码**：常驻进程（不退出）。
- **期望输出关键片段**（stderr）：
  ```
  relay trunk: ON — the relay protocol shares the direct-link port (UDP ...)
  ```
  没有 `--relay-trunk` 时必须是：
  ```
  relay trunk: off (pass --relay-trunk to carry relays between members)
  ```
- **反向检查**：`curl -sf http://<host>:<port>/healthz` → 200。

### T2. 一条命令入网（凭证式，外部代理跑）

```bash
cmesh join --cred "$CRED" --state "$MY_STATE" --device-passphrase "$PASS" \
  --relay-trunk "$ANCHOR_ADDR" --relay-trunk-id "$ANCHOR_ID" \
  > /tmp/join.log 2>&1 &
sleep 5
```

- **期望退出码**：常驻进程（不退出）。若要一次性验证入网成功，加 `--once`（入网即退，退出码 0）。
- **期望输出关键片段**（stderr）：
  ```
  created device identity <deviceID>
  enrolled as device <deviceID>
  Enrolled with a team credential — no approval needed.
  approved: this device is now a member
  ```
  （`created device identity` 只在**首次**入网时出现——第二次入网时该目录已有 `device.json`。）
- **失败时的三种典型原文**：
  ```
  could not enroll at the discovery node: <原因>
  enrollment refused: credential quota exhausted: N of M devices already joined with it
  could not enroll at the discovery node: 127.0.0.1:PORT: unreadable answer (HTTP 404)
  ```
  最后一条 = **锚点没带 `--account-pub`**（enroll 端点根本没挂载）→ 找值守，不是产品错。

**邀请式 join（仅公网拓扑）**：

```bash
cmesh join --listen 0.0.0.0:7000 --state "$MY_STATE" --device-passphrase "$PASS" "<invite>"
```

- **期望退出码**：0（打印 `PUBKEY` 后等待批准）。
- ⚠️ **邀请串必须放最后**（§4.1）；**私网拓扑下这条路走不通**（§7.5 N-1）。

### T3. 成员视图（`status`）

```bash
cmesh status --state "$MY_STATE" --device-passphrase "$PASS"
echo "rc=$?"
```

- **期望退出码**：0。
- **期望输出关键片段**：
  ```
  account: <accountID>  view: v<N>  online: <n>/<m>
    <deviceID>      online   reachability=unknown endpoints=...
  ```
  **注意**：**没有 `--probe` 时 `reachability` 必须是 `unknown`**（`internal/cli/status.go:204-206`
  ——发现层只证明成员资格与端点候选，不证明可拨）。写成 `direct` 才是伪造。

### T4. `status --probe`（带真实传输证据）

```bash
cmesh status --state "$MY_STATE" --device-passphrase "$PASS" --probe "$PEER_ID"
```

- **期望退出码**：0。
- **期望输出关键片段**：该成员那行 `reachability=direct`（直连可达）或 `reachability=relay`（经干道）。
- **失败时**：`reachability=degraded` + `probe_error=<原因>`。

### T5. `mesh ping`（直连与中转）

```bash
# 直连
cmesh mesh ping --count 30 --state "$MY_STATE" --device-passphrase "$PASS" "$PEER_ID"
echo "rc=$?"

# 中转（纯出站对纯出站）
cmesh mesh ping --count 3 --state "$MY_STATE" --device-passphrase "$PASS" "$PEER_ID"
```

- **期望退出码**：0。
- **期望输出关键片段**（直连）：
  ```
  --- <deviceID>: 30/30 pings, lost 0 (0.0%), p50 0.xxxms p95 0.xxxms, link=direct
  ```
- **期望输出关键片段**（中转）——**这一行是中转干道的判定钥匙**：
  ```
  --- <deviceID>: 3/3 pings, lost 0 (0.0%), p50 xx.xxxms p95 xx.xxxms, link=relay via=<锚点deviceID>
  ```
- **注意**：p95 ≥ 5ms 只会打一条 stderr 提示（`internal/cli/meshcmd.go:491-493`），
  **不是失败**——P-1 的 5ms 门槛只对同机链路有意义。

### T6. `msg`

```bash
cmesh msg --text "hello 网格" --state "$MY_STATE" --device-passphrase "$PASS" "$PEER_ID"
echo "rc=$?"
```

- **期望退出码**：0。
- **期望输出关键片段**（发送端 stderr）：
  ```
  delivered 1 message(s) to <deviceID>
  ```
- **对端 join 进程的 stderr**：
  ```
  📩 from <deviceID>: hello 网格
  ```
- **中文与多行**必须逐字节一致。

### T7. `send-to`

```bash
head -c 1048576 /dev/urandom > /tmp/f.bin
cmesh send-to --file /tmp/f.bin --state "$MY_STATE" --device-passphrase "$PASS" "$PEER_ID"
echo "rc=$?"
```

- **期望退出码**：0。
- **期望输出关键片段**（发送端 stderr）：
  ```
  sent f.bin (1048576 bytes) to <deviceID>
    sha256 <hex> — receiver verified
  ```
- **对端 join 进程的 stderr**：
  ```
  📬 from <deviceID>: /…/inbox/f.bin → <path> (sha256 …, 1048576 bytes)
  ```
- **字节完整性判据**：对端落盘文件与源文件 **sha256 逐字节相等**（`sha256sum` 两边对拍）。
- **中转场景**：文件 ≥1 MiB 也必须通过（曾经「>1 MiB 经中转必失败」是记录上限导致，已修）。

### T8. `run`（内置只读动词）

```bash
cmesh run --state "$MY_STATE" --device-passphrase "$PASS" "$PEER_ID" @info
echo "rc=$?"

cmesh run --state "$MY_STATE" --device-passphrase "$PASS" "$PEER_ID" @cat /etc/hostname
```

- **期望退出码**：0。
- **期望输出关键片段**（`@info`）：
  ```
  hostname  <host>
  os       linux/amd64
  version  <cmesh version>
  go       go1.x
  ```
- **`@cat` 的字节判据**：对 4095 / 4096 / 4097 / 65536 / 1048576 B 逐个测，输出长度必须**精确等于**输入长度。
- **远端退出码透传**：远端动词失败时，请求方拿到**远端的**退出码（ssh 语义）。

### T9. `bench`

```bash
cmesh bench --size 4MB --state "$MY_STATE" --device-passphrase "$PASS" "$PEER_ID"
echo "rc=$?"
```

- **期望退出码**：0。
- **期望输出关键片段**：
  ```
  peer=<deviceID>
  direction=both size_each=4194304 sent=8388608 received=8388608
  duration=…s throughput_each=…MiB/s packets_lost=0
  ```
  **`sent` 必须等于 `received`**——不等就是字节完整性失败，不是「性能问题」。
- **非对齐尺寸**要另跑一轮（4095 / 4097 / 65537 / 1048577 / 33554431），
  断言 `sent == received` 且两端 sha256 一致。

### T10. 撤销拆流（值守在锚点跑；§3.1 说明为什么不能在成员设备上跑）

```bash
# 锚点：吊销整个凭证（级联踢出它带进来的全部设备）
cmesh cred revoke --state "$ANCHOR_STATE" --cred-id "$CRED_ID"
# NOTE: no --account-passphrase. The revocation acts on the ANCHOR's roster
# file, not on the account key — the code says so on purpose
# (internal/cli/cred.go), and the flag does not exist.
echo "rc=$?"
```

- **期望退出码**：0。
- **期望输出关键片段**（stderr）：
  ```
  credential <ID> revoked.
    N device(s) left the network:
      <deviceID>
    its id is on the revocation list: …
    the discovery node picks the change up from <path>/members.json within a second.
  ```
- **成员侧的观察**（外部代理跑）：被踢的成员下一次命令报
  `this device has been removed from this network (revoked by the account owner, or the approval never happened)`。

### T11. `cred list`（值守在锚点跑）

```bash
cmesh cred list --state "$ANCHOR_STATE"
# NOTE: no --account-passphrase either — the list reads the roster file.
echo "rc=$?"
```

- **期望退出码**：0。
- **期望输出关键片段**（stderr）：
  ```
  credentials in <path>/members.json
    <credID>   members=2   quota=10  expires=2027-…Z  valid
  revoked credentials in <path>/members.json
    <credID>   revoked (no device can enrol with it)
  ```
- **在成员设备上跑**必然是 rc=1 + §3.1 那句指路文本——**这不是失败项**。

### T12. 面板黑盒（外部代理跑，令牌由值守给）

```bash
export PANEL=http://<anchor-host>:<port>
export TOK='<面板令牌>'

curl -s -o /dev/null -w '%{http_code}\n' "$PANEL/panel"                       # 期望 308
curl -s -o /dev/null -w '%{http_code}\n' "$PANEL/panel/"                      # 期望 200
curl -s -o /dev/null -w '%{http_code}\n' "$PANEL/api/mesh"                    # 期望 401（无令牌）
curl -s -o /dev/null -w '%{http_code}\n' "$PANEL/api/mesh?token=$TOK"       # 期望 401（查询串不算）
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Panel-Token: $TOK" "$PANEL/api/mesh"  # 期望 200
```

- **期望退出码**：curl 本身 0；看 `-w` 打印的 HTTP 码。
- **期望输出关键片段**（最后一个请求的 body）：
  ```json
  {"account_id":"…","version":7,"summary":{"members":3,"online":2},"members":[…]}
  ```
- **308 必须带 `Cache-Control: no-store`**：否则浏览器缓存过的 308 会让「撤掉重定向看红」读出假数据。

### T13. systemd（外部代理跑，需 root）

```bash
sudo systemctl enable --now cmesh-discovery.service
systemctl is-active cmesh-discovery.service          # 期望 active
systemctl show -p ExecMainStatus cmesh-discovery.service
sudo kill -TERM "$(systemctl show -p MainPID --value cmesh-discovery.service)"
sleep 2; systemctl show -p ExecMainStatus cmesh-discovery.service   # 期望 0（不是 143）
sudo kill -9 "$(systemctl show -p MainPID --value cmesh-discovery.service)"
sleep 6; systemctl is-active cmesh-discovery.service                # 期望 active（自动拉起）
```

- **期望退出码**：命令本身 0。
- **期望输出关键片段**：
  ```
  ExecMainStatus=0
  ```
- **`--account-pub` 必须在 cmdline 里**（否则 `/mesh/enroll` 根本不挂载，凭证入网不可用）：
  ```bash
  tr '\0' ' ' < /proc/$(systemctl show -p MainPID --value cmesh-discovery.service)/cmdline
  ```
  期望看到 `--account-pub <hex-or-b64>`。

### T14. 安装与校验（外部代理跑）

```bash
curl -fsSLO "https://github.com/muzimu217/container-mesh/releases/download/v1.0.0-rc2/cmesh-v1.0.0-rc2-linux-amd64"
sha256sum -c SHA256SUMS --ignore-missing 2>/dev/null | grep 'linux-amd64: OK'   # 期望有一行 OK
cmesh verify --file ./cmesh-v1.0.0-rc2-linux-amd64 --sha256 <期望的 hex>       # 期望 rc=0
# 篡改后必须被拒
printf 'x' | dd of=./cmesh-v1.0.0-rc2-linux-amd64 bs=1 seek=100 conv=notrunc 2>/dev/null
cmesh verify --file ./cmesh-v1.0.0-rc2-linux-amd64 --sha256 <期望的 hex>; echo "rc=$?"   # 期望 rc=1
```

- **期望退出码**：匹配 0 / 被篡改 **1**（且输出含 `does NOT match the published digest`）。
- **注意**：`--ignore-missing` 会**掩盖「清单承诺但文件不存在」**——必须确认**自己那行**显示 `OK`。

### T15. Release 资产完整性（外部代理跑）

```bash
gh release view v1.0.0-rc2 --json assets -q '.assets[].name' | sort > /tmp/assets.txt
curl -fsSL "https://github.com/muzimu217/container-mesh/releases/download/v1.0.0-rc2/SHA256SUMS" \
  | awk '{print $2}' | sed 's/^\*//' | sort > /tmp/sums.txt
diff /tmp/assets.txt /tmp/sums.txt && echo "清单与资产一一对应"
```

- **期望退出码**：0（无 diff）。
- **期望输出关键片段**：`清单与资产一一对应`。
- **已知**：rc1 曾是「15 行清单对 10 个资产、7 行悬空」，`--ignore-missing` 恰好跳过它们 →
  「校验通过」是假象。**rc2 才是完整替代**。

---

## 6. 证据格式规范

### 6.1 一个 PASS 要带什么

每条 PASS **必须**带这四样，缺一不可：

| # | 内容 | 说明 |
|---|---|---|
| 1 | **完整命令原文** | 包含变量展开后的真实值或「`$VAR` 已在环境里定义」的声明。**不能**只写「跑了 ping」。 |
| 2 | **退出码** | `rc=0` / `rc=1` / `rc=2` 等。**rc=2 是用法错误，要单独归类**，不与运行时失败混在一起。 |
| 3 | **输出关键片段** | 判定所依赖的那几行（§5 每个模板都给了「必须出现的片段」）。不要贴 500 行日志。 |
| 4 | **环境与拓扑** | 机器、系统、架构、网络形态（公网 / 私网 / netns / 回环）、被测 SHA。 |

**加分项**（有就更好）：

- 完整 stdout/stderr 的**首 20 行与末 20 行**（中间可省略，但**不许**只给末 20 行）。
- 拓扑图或 `ip route` / `ip addr` 的关键几行。
- 重复次数（`ping --count 30` 是 30 次采样，单次成功不作数）。

### 6.2 报告格式（照抄这个骨架）

```
### [矩阵编号] <能力名> — PASS / FAIL / 未验

- 环境：<机器 / 系统 / 架构>，拓扑：<公网|私网|netns|回环>
- 被测 SHA：<sha>
- 命令：
      <命令原文>
- 退出码：<rc>
- 输出关键片段：
      <必须出现的那几行>
- 判定：<为什么这一段足以判定>
- 备注：<环境限制 / 未验的部分 / 与上一轮的差异>
```

### 6.3 「未验」怎么写（而不是写成「不适用」）

**「不适用」是错的**，它把「我没测」伪装成「这里没东西可测」，会让缺口永远消失。
正确写法是 **「未验」+ 缺什么 + 需要谁配合**。

| 写法 | 含义 | 是否接受 |
|---|---|---|
| `PASS` | 真跑了，判据达成 | ✅ |
| `FAIL` | 真跑了，判据没达成 | ✅ |
| `未验 — 缺环境：<具体缺什么>` | 跑不了，因为环境/权限不具备 | ✅ **正确** |
| `未验 — 需网主授权：<需要什么>` | 跑不了，因为缺授权 | ✅ **正确** |
| `未验 — 缺值守配合：<需要值守给什么>` | 跑不了，因为缺前置输入 | ✅ **正确** |
| `不适用` | ❌ **不接受** | ❌ |
| （不写） | ❌ **不接受**——静默让人无法区分「没做」和「做不了」 | ❌ |

**硬规矩**：**被环境卡住就直接说卡点，不要静默**。
kuku / netns 曾连续多轮静默 50 h+，协调者无法区分「你没做」和「你做不了」——
最后只能按失联结案、任务永久改派（`docs/ROUND-LOG.md` Round 69）。

### 6.4 三态的处置

| 状态 | 处置 |
|---|---|
| PASS | 记账，标被测 SHA |
| FAIL | **先自己复核**是不是 §4 的坑（flag 顺序 / 环境变量 / 拓扑伪影 / 测量口径），再上报。**上报必须带 §6.2 的完整格式** |
| 未验 | 写清缺什么 + 需要谁配合，**不阻塞其它项的回报** |

---

## 7. 本轮暴露的缺陷与已修项清单

> 来源：`docs/ROUND-LOG.md`（Round 8–70）与 `docs/decisions.md`（D73–D78）。
> **判据**分三类：
>
> - **产品缺陷** = 产品行为与自己的契约/需求不符（文档写了、代码没做，或代码做错了）。
> - **测试拓扑伪影** = 现象由测试环境的构造方式造成，换个拓扑就没了。
> - **环境限制** = 现象由沙箱/网络/权限的客观边界造成，产品无可改进空间。

### 7.1 产品缺陷（已修）

| # | 现象 | 判据 | 修法 | 出处 |
|---|---|---|---|---|
| B-1 | 委派成员能入网、在名册，**谁都拨不通** | 「能入网」与「可拨」是两条独立契约（R-1.1 / R-3.1），前者成立不推出后者 | 用快照携带的委派记录验签 | Round 60 |
| B-2 | `--endpoint host:0` 被原样公告，锚点判畸形 | 公告的端口必须是**实际监听**的端口 | `resolvePortZeroEndpoints` | Round 60 |
| B-3 | 凭证设备第二条命令起全废（`saved invite is malformed`） | 一条命令入网的产品形态要求**后续所有命令都能跑** | 配置改存派生字段，旧格式回退 | Round 60 |
| D-7 | 跑一次 `mesh ping` 后本机 30s 不可被直连 | 「谁在公告」必须唯一（D-10 精神） | `NoAnnounce` 视图模式 + 空端点不覆盖已公告端点 | Round 10 |
| F-1 | 一次性命令后被用成员的 anchor 推送速率 **158 B/15s → 15.8 MB/15s 且无界** | 关闭的 channel **永远可读** → 旧订阅者立即永久触发 → 热循环 | 消费侧判 `!ok` 退出（不改 `Subscribe` 语义） | Round 49，四方独立验证闭环 |
| — | 多端点 fallback **整体失效** | quic-go 的 single-use Transport：一次拨号失败后 socket 已关，后续端点全在死 socket 上 | 每次尝试单独 bind 一个 socket | Round 50 |
| — | `bench` 在真实公网上间歇报 `invalid bench frame size` | 读超时落在帧中间，重试从帧中间当新帧头 | 帧读取器带跨超时的部分字节累积 | Round 60 |
| — | `isRateLimited` 拿错误文本里的「429」判定限流 | 节点的**端口号**里含 429 就被误判 → 入网悄悄慢 3 倍 | 判定改从 HTTP 状态码取，文本只作兜底 | Round 61 |
| — | `join --timeout` 实为**整个成员进程**的期限 → 默认 10 分钟后成员自动退网 | 帮助文本写的是「等审批的期限」 | 超时只约束等审批那一段 | Round 61 |
| — | 成员证书过期 = **静默掉线**（无错误、无续期） | 入网即常驻（D77）：成员不该因为时间流逝消失 | 成员自带续期循环（凭证剩余寿命 1/4 处，间隔限 1h–30d） | Round 67 |
| — | `cred revoke` 先 `LoadMembers` 把 ENOENT 上抛 → **新装锚点**上预吊销执行不了 | 「泄漏但还没人用」恰恰最常发生在早期 | 名册缺失视为空名册（`LoadMembersOrEmpty`） | Round 69 |
| — | `cred list` **不显示吊销列表** | 运维没有任何 CLI 手段确认吊销了什么 | list 末尾追加吊销一节 | Round 69 |
| — | 手写名册 `"revoked_creds": []` 让**所有** cred 命令不可用 | 人写「没有吊销」的直觉写法不该废掉整个名册 | 空的数组被修复成空对象 | Round 69 |
| — | **吊销会被锚点自己的下一次保存抹掉**（安全缺陷） | root 写的 0600 文件锚点的非特权用户读不了 → 热重载失败 → 写回时抹掉吊销 → 被吊销的凭证又能入网 | 新文件继承目录属主 + 保存时合并文件里的吊销（只增不减） | Round 68 |
| — | 中转**端到点从未到达请求方** | `changed` 的比较写在写入之前 → 永远说「没变」→ 从不推视图 | 端点纳入比较、写入挪到比较之后 | Round 61 |
| — | 半关闭被当成整流结束 → **投递回执在中转回程上被丢** | 任一方向结束就拆流，会把还在路上的回执一起关掉 | 等两个方向都结束；读方向 EOF 只半关 | Round 61 |
| — | 修复上一条引入的回归：**配额槽永久泄漏**，干道 20 次后彻底堵死 | 单测测不出「流永不结束」 | 目标处理完就半关自己的会话 | Round 61 |
| — | 撤销/超限**没有真正掐断流量** | 礼貌 FIN 可以被无视 | 新增可选 `sessionAborter`（`CancelRead/CancelWrite`），正常收尾半关、强制拆除才 abort | Round 61 |
| — | 经中转的大文件必失败（`record too large`） | 1 MiB 分块封签后 1.4 MB > 记录上限 | 写侧按记录上限拆分 | Round 61 |
| — | **假投递回执**（发送端报「已投递」，目标根本没收到） | 把任何读错误都当正常半关 | 区分「干净半关」与「真失败」，失败回 `err: <原因>` | Round 61 |
| — | joiner 表竞态 → 目标重连后**中转永久失效**（只有重启锚点才恢复） | `Remove(id)` 无条件 delete，旧连接的清理把新 joiner 一起删 | `Remove` 改为**按身份检查** | Round 70 |
| — | 速率配额变成**文件大小上限**（49 MiB 过 / 50 MiB 挂） | P-7 是**速率**（≤50 MiB/s），实现按固定一秒窗口计数 | 改为令牌桶 | Round 70 |
| — | 多中转方的兜底链**只有一跳有效** | 「这个中转方够不着那个目标」是**本地状况**（可回退），与「对方不是成员」（判决）被塞进同一个类 | 新增 `RelayClassUnavailable` → `ErrRelayUnavailable`（可回退） | Round 70 |
| — | `cmesh relay` SIGTERM 未接线优雅停止（exit 143、6ms 硬死） | 库层 `Server.Shutdown` 已实现且有测试，CLI 未接线 | `signal.NotifyContext` + `srv.Shutdown(10s)` | Round 45（K-1） |
| — | 安装器的版本目录是 **0700** → systemd 以 `User=cmesh` 跑**官方装的二进制**会 Permission denied | unit 的 `User=` 与安装器的目录模式互相矛盾 | 提交前把暂存目录放宽到 0755 | Round 57 |
| — | systemd unit **五处缺口** | 缺 `--account-pub` → **入网端点根本不挂载**（凭证入网在旧 unit 上是真的死的） | 补 `--account-pub` / `StateDirectory` / `TimeoutStopSec` / 去掉 `EnvironmentFile=-` 的 `-` / 显式 `KillSignal` | Round 51 |
| — | `packaging/install_test.sh` fixture 硬编码 darwin-arm64 → linux/amd64 必 rc=1 | 测试装置缺陷，不是产品缺陷 | fixture 按 `uname` 解析平台，并接入 CI | Round 38（F-1） |
| — | `e2e 第 8 节依赖 mkfifo` → 沙箱 seccomp 拦掉 → 每个容器化代理跑不了完整 CI 路线 | 用失败的 `mkfifo` 塞进 PATH，脚本里 shell 建**普通文件**，重定向立刻 EOF | 改用**普通管道** | Round 59 |
| — | 出口策略只实现了一半 → **SSRF 形态** | 字面 IP 检查在，**解析后**地址无人复核（主机名可穿透） | 三条出站路径全补（members WS / register HTTP / enroll HTTP），用 `Dialer.Control` 在 DNS 后连接前判定 | Round 51–53 |
| — | `announce` 侧与配置文件读的是**两个不同的端点列表** → 每个一次性命令都 announce `:0` → 成员在所有人视图里短暂离线 | 「保存什么」必须等于「公告什么」 | 配置文件存**解析后**的列表 | Round 55 |

### 7.2 测试拓扑伪影（不是缺陷）

| # | 现象 | 判据 | 结论 | 出处 |
|---|---|---|---|---|
| T-1 | 「≥256 KiB 的文件经中转会挂死」 | 同一台机直连 3 MiB 只要 4.7 秒；换主机后 **44 B/64 K/256 K/1 MiB 全部通过，3 MiB 22s 通过** | **不是阈值问题，是测试拓扑的 hairpin 伪影**。决定性 A/B：私网宣告 **4/4 成功**，公网宣告 **1 挂 3 过** | D78 第 2 条 |
| T-2 | 「客户端 UDP 校验和卸载故障」 | 关掉 `rx`/`gro` **没有修复**；后来发现是**读错了 `/proc/net/dev` 的列**（第 2 列是 `rx_packets`，第 3 列才是 `rx_errs`） | 容器的真实接收错误是 **0**。推翻 | ROUND-LOG |
| T-3 | 「GSO 超级包超 MTU 被丢」 | 关掉 GSO 后照样抓到 20480 字节 | 那只是 tcpdump 在 `-i any` 上看到的**分段前形态**，上线仍是 1500 字节包 | ROUND-LOG |
| T-4 | 「Reno 拥塞窗口下限导致大文件挂」 | 把窗口从默认（流 512 KiB / 连接 768 KiB）提到流 2 MiB / 连接 16 MiB，**没有修复挂死** | 不是根因（但默认值对中转记录确实太小，改动仍保留） | ROUND-LOG |
| T-5 | 「锚点 → 纯出站成员 `network is unreachable`」 | 私网端点公告 + **无入站映射** = 物理边界；改由另一台成员发起就 10/10 通 | **预期不可达**，不是缺陷 | **D75** |
| T-6 | 「安全组挡住所有入站 UDP」 | 探针用的是 4500/39200（**没开的端口**）；换到 18090 就收到 UDP | 测错了端口。网主早开过 18090 | Round 60 |
| T-7 | 「成员 RSS 75–85 MB，超 15 MB 目标」 | 那是 **GC 之前的相位**；post-GC 实测 **12.0 MB** | 判据用 post-GC 稳态 → **达标** | **D74** |
| T-8 | 「kill -9 后锚点日志没有下线记录」 | 锚点默认日志级别 **Warn**，快速离线路径**不打日志** | 判据改用**面板 online 计数**；实测 HEAD 0.1s / 历史 SHA 0.2s | Round 56 |
| T-9 | 「4 MB/16 MB bench 全 FAIL」 | 是**自己的 250 秒超时**；单独跑 4 MB 只要 6.3 秒 | 测吞吐要给足超时 | Round 62 |
| T-10 | 「`account create` 把 account.json 落在子目录」 | `account create` 写的就是 `<state>/account.json`；对方把不同命令的 `--state` 指向了不同目录 | **用法错误**，不是产品缺陷 | Round 49 续二 |

### 7.3 环境限制（不是缺陷，如实标「未验」）

| # | 限制 | 判据 | 影响 | 出处 |
|---|---|---|---|---|
| E-1 | **reboot 恢复**未验 | 需要重启生产服务器 | F8 标「未验 — 需网主授权」 | Round 37 / DEV-KPI |
| E-2 | **跨云 UDP 被两边安全组全封** | 175 上 tcpdump 抓到 0 个来自对端的包 | 只能走 UDP-over-TCP 隧道，RTT 含隧道开销 | Round 62 |
| E-3 | **公网 UDP 在 ~7–9 Mbps 开始丢包** | 裸 UDP 探针（不经本仓代码）+ 逐层差分（`Ip.InReceives` → `Ip.InDelivers` → `Udp.InDatagrams`）两套独立探针数据一致 | P-3 的 200 MiB/s 在公网口径下**物理不可能** | Round 62 |
| E-4 | **hub 公网出口带宽 ≈5.5 Mbps** | 与本仓无关的 HTTPS ranged download 对照，三次干净复现 | 中转吞吐按「实际带宽可达」标注，不按 P-3 卡 | Round 59 / D73 |
| E-5 | **无 root / 无 netns / 无 systemd** 的沙箱 | 外部代理如实申报 | 只能做回环等价形态；NAT 方向性、systemd 项标「未验」 | Round 38 |
| E-6 | **mkfifo 被沙箱 seccomp 拦**（EPERM） | kuku 原始环境 | 曾使 e2e 变成 42/1；已用管道替代修复 | Round 51 / Round 59 |
| E-7 | **`windows/arm64` 资产完全不存在** | `gh release view` 实测 10 个资产、矩阵需 20 | 平台矩阵标「未验」 | Round 50 |
| E-8 | **无公网锚点**（部分外部代理的环境） | oh-no 明确申报 | 「register 负向在真公网 IP 正向下」这条**未测** | Round 55 |
| E-9 | **quic-go 无每流重传计数**的说法 | 性能代理推翻：暴露的是**每连接**计数（`ConnectionStats().PacketsLost` + `Tracer.LostPacket`） | 计数是**丢包数**不是「重传数」，字段已改名 `PacketsLost` | Round 57 / DEV-KPI |
| E-10 | **跨账号中转判据缺失** | 视图缺每成员的账号归属 | 跨账号中转标「未验 / 未实现」 | Round 61 补记 / Round 70 |

### 7.4 「判据用错」类（历史上最常见的一类误报）

这一类单独列，因为它**最费时间**：现象是真的，但根因是**测量装置**错了。

| # | 上报的现象 | 正确的判据 | 出处 |
|---|---|---|---|
| M-1 | 「凭证成员 kill -9 后 70–90s 才下线」 | **面板 online 计数**（不是锚点日志）；按此判，实测 **0.1–0.2s** | Round 56 |
| M-2 | 「e2e 第 8 节失败」 | 是 `mkfifo` 被拦 → 脚本用普通文件重定向 → 第二条消息根本没发 | Round 59 |
| M-3 | 「安全组挡住所有入站 UDP」 | 先**换端口**再下结论 | Round 60 |
| M-4 | 「RSS 未达标」 | 先分清 **pre-GC / post-GC 相位** | D74 |
| M-5 | 「大文件经中转挂死」 | 先看**是握手层还是传输层**（「read verdict: deadline exceeded」指向传输，实际原因在握手） | ROUND-LOG 教训② |

### 7.5 本轮整理时新发现的缺口（**已修 —— 验收时不必再绕开**）

> 这一节是**写这份清单时现场实测出来的**，不在任何历史轮次记录里。
> 两条都曾是真缺陷，**但它们在 Round 104 之前已经被修掉了**
> —— **下面保留原文，是因为「修好了」这句话本身需要有据可查。**
>
> ★ **为什么保留这一节而不是删掉**：一份验收清单里「曾经要绕开的地方」，
> **如果连同绕开方式一起删掉，下一个测试员会重新踩一遍**，
> 而踩的人不会知道这条路曾经被标过「已修」。

| # | 现象 | 判据 | 影响 | 现在的状态 |
|---|---|---|---|---|
| **N-1** | **`CMESH_ALLOW_PRIVATE_DISCOVERY=1` 对邀请式 join 不生效** | 开关只在 `normalizeBaseURLWith` 这条**可切换**的路径上生效（`internal/discovery/client.go:1039`）。`DecodeInvite` 调的是**不可切换**的 `normalizeBaseURL`（`internal/discovery/member.go:400`）→ 邀请串里的私网/回环发现地址**一律被拒**，与开关无关。[实测]：开关已 export 的情况下 `join "<invite>"` 仍报 `bad invite: discovery: malformed endpoint: "127.0.0.1:64866"` | **私网部署走邀请式 join 走不通**。而凭证路径走的是切换版（`internal/cli/join_cred.go:101`）→ **凭证式正常** | **✅ 已修**：`DecodeInvite` 现在走 `normalizeBaseURLWith(a, privateAllowed)`（`internal/discovery/member.go:428`）—— **邀请式 join 与凭证式 join 现在走同一条可切换的路径**。<br>**守卫**：`internal/discovery/invite_private_test.go`（4 条）+ `internal/discovery/private_discovery_test.go`（5 条）<br>**⇒ 私网 / netns / 回环拓扑现在两条路径都能用，不再需要「一律用 `join --cred`」** |
| **N-2** | **`cmesh invite` 生成邀请串时不校验发现地址** | `runInvite`（`internal/cli/discovery.go:211-217`）只检查 `--disc` 非空，**不**做 `normalizeBaseURL` 校验 → 它能生成一个**自己 join 不了**的邀请串（私网地址）。[实测]：`invite --disc 127.0.0.1:PORT` 正常打印，但随后 `join` 该 invite 报 `malformed endpoint` | 邀请串**在生成侧通过、在使用侧被拒**，报错信息指向「邀请串坏了」而不是「这个地址不被允许」——排障方向被带偏 | **✅ 已修**：`runInvite` 在打印邀请串**之前**就校验地址（`internal/cli/discovery.go` 的 "The addresses are VALIDATED before the invite is printed" 段）—— **它生不出一个自己 join 不了的邀请串**。<br>**⇒ 「先自检地址是否可达」这条绕开方式不再需要** |

**为什么这两条值得单列**：它们是「**结构上够不到**」家族的新成员——
和 §3.1（名册只在锚点）、§3.4（锚点不能主动发起）同类：
**能力存在，但某条路径上够不到，且报错不指向真正的解法**。

**判据写法**：这类问题的特征是「同一份配置，生成侧接受、使用侧拒绝」。
遇到「我生成的东西怎么用不了」，先想这条，而不是怀疑命令写错了。

---

## 8. 给外部测试员的开工信（可直接复制发出）

```
本轮复测对象：container-mesh M4（可靠网），被测 SHA = <sha>。

【先做这三件事，否则你会卡住】
1. 取环境变量：CMESH_PASSPHRASE（≥8 字符口令）。私网/netns/回环拓扑再加
   CMESH_ALLOW_PRIVATE_DISCOVERY=1（每一条拨号锚点的命令都要带）。
2. 向值守要三样东西：① 锚点签发的凭证 cmesh1.…（附凭证 ID）② 锚点的 --account-pub
   ③ 锚点的 --relay-trunk-id（deviceID）。没有①你连网都进不去。
3. 拉源码用 codeload tarball（git fetch 会间歇超时）：
   curl -fsSL -o cm.tar.gz "https://codeload.github.com/muzimu217/container-mesh/tar.gz/refs/heads/develop"
   核对 SHA 与本单一致。

【flag 顺序】deviceID 一律放最后：msg/send-to/mesh ping 都是 `… --flag v <deviceID>`。
位置参数放前面会被 Go 的 flag 解析吞掉（rc=2）。

【Docker 拓扑】锚点必须 --announce 私网地址（如 10.0.0.14:18090）。
宣告公网地址会让流量绕云 NAT hairpin，bulk UDP 会大量丢失 → 假的「中转挂死」。
容器要 --platform linux/amd64。

【做不了就直说】缺环境/缺授权就写「未验 — 缺 X，需要 Y」，不要静默，也不要写成「不适用」。
静默会让协调者无法区分「你没做」和「你做不了」。

【回报格式】每条 PASS 必带：完整命令原文 / 退出码 / 判定所依赖的输出片段 / 环境与拓扑 / 被测 SHA。
rc=2 是用法错误（命令写错了），rc=1 是运行时失败（环境或状态不对），两者不要混。
```

---

## 9. 本文与其它文档的关系

| 想知道 | 读 |
|---|---|
| 要什么能力（需求编号、判据） | [`../REQUIREMENTS.md`](../REQUIREMENTS.md) |
| M4 交付到哪一步 | [`../PLAN.md`](../PLAN.md)、[`../DEV-KPI.md`](../DEV-KPI.md) |
| 每轮发生了什么（缺陷出处） | [`../ROUND-LOG.md`](../ROUND-LOG.md) |
| 为什么是这个设计（决策与被推翻的方案） | [`../decisions.md`](../decisions.md)（尤其 D73–D78） |
| 怎么部署（锚点/成员/Docker/systemd） | [`../../deploy/MESH-NET.md`](../../deploy/MESH-NET.md) |
| systemd 单元的前置 | [`../../packaging/systemd/README.md`](../../packaging/systemd/README.md) |
| 凭证的信任模型（为什么要两段入网） | [`CREDENTIAL.md`](CREDENTIAL.md) |
| 验收判据的纪律来源 | [`../CONVENTIONS.md`](../CONVENTIONS.md) §2（测试纪律） |
| 多宿主设计（未来形态，不在 M4 验收范围） | [`MULTI-HOMING.md`](MULTI-HOMING.md) |
