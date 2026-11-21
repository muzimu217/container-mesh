# 设计稿：中转干道记录编码（第一步：并行的二进制格式）

> **状态**：**第一步已完成，第二步未开始。** 本文描述的二进制格式已经写出来、有完整的
> 测试与基准，但**没有任何生产调用点使用它**：`writeRelayRecord` / `readRelayRecord` /
> `forwardRecords` 一行都没改。切换是下一步的事。
>
> **版本**：D1（2026-11-19）
>
> **数字标注**遵循 T-5.3：`[实测]` / `[推断]`。本文所有 `[实测]` 数字都指到可复现的
> 命令。

---

## 0. 摘要

1. **问题**：锚点在转发每一条中转记录时，先 `json.Unmarshal` 再 `json.Marshal`。
   JSON 把 `[]byte` 编码成 base64，所以锚点对每一条它**本来就不该读**的密文做了
   解码、base64 解码、重编码、base64 编码。实测锚点 CPU 的 **34.89%** 花在这件事上
   （32 MiB 过中转，20 次传输，40 s 窗口，12.01 s 采样：`json.Unmarshal` 23.98%（其中
   base64 9.74%）、`json.Marshal` 10.91%（其中 base64 9.08%））。

2. **做法**：定长 24 字节头 + 长度前缀变长字段的二进制记录格式。头的第 3 字节是
   **格式 tag `0xC7`**，旧解析器遇到它必然响亮失败。

3. **字节** `[实测]`：同载荷下二进制帧比 JSON 小 **25.0%**（1 KiB: 24.33%，32 KiB:
   24.98%，256 KiB: 25.00%）。同样 1 MiB 的记录预算，二进制能多装 **33.38%** 的载荷。

4. **CPU** `[实测]`（Apple M4，darwin/arm64）：32 KiB 载荷的编解码往返，
   JSON **110,209 ns/op、83,161 B/op、5 allocs**；二进制 **3,414 ns/op、40,960 B/op、
   1 alloc**。**快 32.3×，少分配 2.03× 的内存。**

5. **更大的价值不在字节数**：二进制格式让锚点可以**真正逐字节转发**——
   读 24 字节头，剩下的一律不碰。`forwardRecords` 现在的注释写着 "forwarded
   VERBATIM"，但代码实际是 decode + re-encode（`relaytrunk.go:1214` 读、
   `relaytrunk.go:1254` 写）。二进制格式把这个注释变成事实。转发路径实测：
   **106,975 → 12.57 ns/op（8,509×），零分配**。

6. **兼容性**：老锚点 + 新成员 → **响亮失败**（`invalid character`）。老成员 + 新锚点 →
   **取决于 ALPN**，见 §4。

7. **两个必须先说的结构性事实**（都是第二步的前提）：
   - **中转是两条腿**（§4.0）：A ↔ 锚点 ↔ C。两条腿**必须同格式**才能逐字节转发，
     所以「两条腿都是新版」是收益的前提。
   - **版本协商只能靠 ALPN**（§4.3）：记录格式是 A↔C 的协议，而 ALPN 是连接的
     属性。Go 的 ALPN 协商是**服务端优先**的，实测已验证顺序规则（§4.3）。

---

## 1. 帧布局

所有整数大端。长度都是字节数。

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|    magic      |    version    |     type      |     flags     |
|    0xC7       |      1        |   frame type  |  see below   |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                                                               |
+                     session (uint64)                         +
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                           hops (uint32)                       |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                          metaLen (uint32)                     |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                        payloadLen (uint32)                   |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                     meta block (metaLen bytes)                |
|                     ...                                        |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                     payload (payloadLen bytes)                |
|                     ...                                        |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

**定长头 24 字节，永远存在。** `type` 和 `payloadLen` 都在里面：读一次 24 字节就知道
「这是什么、载荷多长」，**不用碰载荷**。这是逐字节转发的前提。

**meta 块**放只有控制记录才用得上的字段，每个都带长度前缀，且只在对应的 flag 位
置位时出现：

| 字段 | 长度前缀 | 出现条件 |
|---|---|---|
| `kind` | uint16 + 字节 | flag `0x04` |
| `reason` | uint16 + 字节 | flag `0x08` |
| `class` | uint16 + 字节 | flag `0x10` |
| `from` | uint16 + 字节 | flag `0x20` |
| `to` | uint16 + 字节 | flag `0x40` |
| `hello` | uint32 + 字节 | flag `0x80` |

**flags 字节**：`0x01` Close、`0x02` Grant、`0x04` Kind、`0x08` Reason、`0x10` Class、
`0x20` From、`0x40` To、`0x80` Hello。

**出现规则与 JSON 的 `omitempty` 一致**：零值 = **不在线上**，不是「在���线但是空的」。
所以二进制帧与 JSON 信封对「零值意味着什么」的判断完全相同，两种格式解出来的记录
必然相等（`TestRelayFrameCrossFormatAgreement` 断言了这一点）。

**`hello` 保持 JSON**：它是签过名的握手消息，签名才是要紧的东西，它的字节不是热
路径。保持 JSON 意味着第二步**完全不用碰 E2E 代码**。

**承载批量数据的 DATA 记录没有任何 meta 字段**——整条记录就是 24 字节头 + 载荷。

---

## 2. 格式 tag 的选择理由

`relayFrameMagic = 0xC7`。

1. **0xC7 不是任何 JSON 值的合法首字符。** JSON 文档/值的首字符只可能是
   `{`、`[`、`"`、`-`、数字、`t`、`f`、`n` 或空白。0xC7 都不是。
2. **0xC7 不是合法的 UTF-8 首字节**（`0xC7 & 0xC0 == 0xC0`，是 2 字节序列的首
   字节），所以任何语言的 JSON 解析器都不可能把它当成文本的开头。
3. 因此**旧解析器必然响亮失败**，错误是 `invalid character 'ç'` 这一类，而不是
   「解析成功但字段错了」。这是本设计最重要的一条兼容性性质。

`TestRelayFrameOldParserFailsLoudly` 断言：把二进制帧喂给 `encoding/json` 的
`Unmarshal`，必须报错，且错误信息必须含 `invalid character`，且失败后产出的记录不
得看起来可用（Type 为 0、Data 为空）。

`TestRelayFrameMagicIsNotAJSONStartByte` 把这个性质钉死：它把 magic 字节逐个比对
JSON 的合法首字符集合，并检查它不是 UTF-8 续字节。**将来谁改了 tag，这个测试会
红**——改 tag 是协议变更，不是重构。

### 为什么不是「让旧解析器能读」

另一种设计是给二进制帧加一个 JSON 前缀，或者让旧解析器先看首字节再分流。前者让
锚点仍然要**同时**认识两种格式（多一份代码、多一条分支）；后者要求旧解析器先改。
本设计选择「旧解析器不改」：混合版本时失败是**响亮的、一次性的、可定位的**，
而不是「旧端悄悄按 JSON 解析出一堆空字段」。

---

## 3. 实测数字

### 3.1 字节

命令：

```bash
go test ./internal/mesh/ -run 'TestRelayFrameJSONAndBinarySizes|TestRelayFrameCarriesMorePayloadPerRecord' -count=1 -v
```

输出：

```
payload    1024: JSON     1385, binary     1048, saving 24.33%
payload   32768: JSON    43709, binary    32792, saving 24.98%
payload  262144: JSON   349545, binary   262168, saving 25.00%
payload  785408: JSON  1047229, binary   785432, saving 25.00%
payload per 1 MiB record: JSON 785408 bytes, binary 1047552 bytes, +33.38%
```

25.0% 不是凑出来的：base64 是 4 字节编码 3 字节，载荷越大这个比例越接近 1/4。1 KiB
时是 24.33%（固定头 24 字节被摊薄得少）。

**第二个数字更重要**：同样 1 MiB 的记录预算，二进制能装 **1,047,552** 字节载荷，JSON
只能装 **785,408** 字节——多 **33.38%**。也就是说切换之后 `maxRelayPlaintext` 可以
从 `maxRelayRecord*3/4 - 1024`（≈786 KiB）涨到接近 1 MiB，**同样的记录条数搬更多的
字节**。

### 3.2 CPU

命令：

```bash
go test ./internal/mesh/ -run '^$' -bench 'BenchmarkRelayRecord' -benchtime=1s -count=1
```

环境：`goos: darwin / goarch: arm64 / cpu: Apple M4 / go1.27.2`。

**往返（编解码各一次）**：

| 载荷 | JSON ns/op | JSON B/op | JSON allocs | Binary ns/op | Binary B/op | Binary allocs | 加速 |
|---|---|---|---|---|---|---|---|
| 1 KiB | 4,282 | 2,963 | 5 | 232.7 | 1,152 | 1 | 18.4× |
| 32 KiB | 110,209 | 83,161 | 5 | 3,414 | 40,960 | 1 | **32.3×** |
| 256 KiB | 853,773 | 667,233 | 6 | 19,595 | 270,336 | 1 | **43.6×** |

**只编码**（32 KiB）：JSON 21,302 ns/op、49,541 B/op、3 allocs；二进制 3,412 ns/op、
40,960 B/op、1 alloc。**快 6.2×。**

**转发路径**（32 KiB 载荷，锚点做的事）：

| 变体 | ns/op | B/op | allocs | 说明 |
|---|---|---|---|---|
| `JSON` | 106,975 | 83,107 | 5 | 今天：Unmarshal + Marshal |
| `JSON/verbatim` | 3,744 | 49,152 | 1 | **对照**：只复制、不解析 |
| `JSON/verbatim+base64` | 88,193 | 82,131 | 3 | **对照**：复制 + 仍要取回密文 |
| `Binary` | **12.57** | **0** | **0** | 读 24 字节头 + 切片，零拷贝 |
| `ForwardStream/JSON` | 110,835 | 133,129 | 9 | 加上 writeFrame 的真实流 |
| `ForwardStream/Binary` | 3,725 | 41,076 | 4 | 加上 writeFrame 的真实流 |

### 3.3 「JSON/verbatim」和「JSON/verbatim+base64」这两行是本文最重要的一张表

`JSON/verbatim` 是**对照实验**：假设只把 JSON 信封的外壳改成二进制、**但保留
JSON 体**，锚点逐字节转发时是 3,744 ns/op（一次 memcpy）。

但这一行**不是可实现的**：锚点转发给下游成员的东西必须是**密文字节**，不是 base64
文本。保留 JSON 体，锚点就仍然必须把 base64 解回字节——也就是**今天它已经在做的
那一件事**。`JSON/verbatim+base64` 这一行就是那个真实成本：**88,193 ns/op**，与
今天的 106,975 ns/op 只差 17.6%，而不是 8,500×。

完整二进制格式是 **12.57 ns/op**（读头 + 切片，零拷贝、零分配）。

**所以：**

- **「第一步就够了吗？」的答案是：不够——但不是差在字节数上。**
- 只做「外层二进制、内层 JSON」，锚点的 CPU **几乎不变**（省 17.6%，而完整格式省
  99.99%）。base64 必须在**锚点这一侧**就被拆掉，否则「逐字节转发」转的是 base64
  文本，**省不下 CPU，只是把成本搬到成员端**。
- 完整二进制格式把转发 CPU 从 106,975 ns/op 降到 12.57 ns/op（**8,509×**），
  而字节只省 25%。**CPU 的收益几乎全部来自「不再解析」，字节的收益来自
  「不再 base64」。** 这是两件独立的事，任何一件单独做都拿不到全部收益。

---

## 4. 混合版本的行为

中转记录走的是 **relay ALPN**（`container-mesh-relay/1`，`relaytrunk.go:2442`），
连接是 mTLS 的（`deviceTLS` + 成员视图 pin）。

### 4.0 先说一个结构性事实：中转是**两条腿**

锚点不是端到端的一根管子，它在中间**转发两个方向**：

```
请求者 A ──── 腿 1 ────▶ 锚点 B ──── 腿 2 ────▶ 目标 C
请求者 A ◀─── 腿 1 ──── 锚点 B ◀─── 腿 2 ──── 目标 C
```

`relayFlow`（`relaytrunk.go:729`）持有 `req`/`tgt` 两条会话和 `reqBr`/`tgtBr` 两个
缓冲读；`forwardRecords`（`relaytrunk.go:1191-1194`）起**两个** goroutine，各跑一条
腿。

**这条事实决定了「逐字节转发」的可行性**：锚点不能把腿 1 的字节原样塞进腿 2，
除非**两条腿用的是同一种记录格式**。腿 1 的字节是 A 写的，腿 2 的字节是 C 读的，
B 在中间只做搬运——所以 A 和 C 必须说同一种话。

而 A 和 C **从来没见过面**，它们之间的唯一约定就是**记录格式**。所以：

> **记录格式是 A↔C 的协议，而 ALPN 只决定 A↔B、B↔C 各自这条连接说什么。**

这条区分是 §4.2 方案取舍的关键。

### 4.1 情形 A：老锚点（JSON）+ 新成员（二进制）

**结果：响亮失败，链路断开。**

新成员发的二进制记录第一个字节是 `0xC7`。老锚点的 `readRelayRecord` →
`json.Unmarshal` 收到 `0xC7` → 报 `invalid character 'ç'`，包成
`mesh: relay: bad record: ...`。老锚点**不会**把它当成一个空记录继续跑——这是
`TestRelayFrameOldParserFailsLoudly` 断言的性质。

失败点在 `forwardRecords` 的 `readRelayRecord`（`relaytrunk.go:1214`）：err 不是
`io.EOF`，于是走 `t.closeFlowForced(flow.id, "relay peer failed")`，链路被拆掉。

**注意这个失败发生在中转记录层，不是 ALPN 层**：TLS 握手是成功的（ALPN 一样），
是第一条记录解析时炸的。日志里看到的是 `bad record: invalid character`，不是
ALPN 不匹配。**诊断时不要往证书/ALPN 方向查。**

### 4.2 情形 B：老成员（JSON）+ 新锚点（二进制）

**结果：取决于 ALPN，而且现状是「握手成功、记录失败」。**

- **连接层不会替你拦住。** ALPN 只有一个值 `container-mesh-relay/1`，新旧都一样。
  Go 的 `negotiateALPN`（`$GOROOT/src/crypto/tls/handshake_server.go:336`）在双方
  列表有交集时正常协商，交集就是 `container-mesh-relay/1`。**没有版本信息可协商。**
- 所以老成员和新锚点**握手成功**，然后老成员发 JSON 记录，新锚点读二进制 → 报
  `not a binary relay frame: bad magic byte 0x7b`（`0x7b` 是 `{`）。
- 链路同样被拆掉，方向与情形 A 相反。

**这意味着第二步必须解决版本协商，否则任何一侧先升级都会打断中转。**

### 4.3 ALPN 的实际协商行为 `[实测]`

Go 的 `negotiateALPN` 是**服务端优先**的：它按**服务端** `NextProtos` 的顺序遍历，
取第一个也在客户端列表里的（`handshake_server.go:345-348`：

```go
for _, s := range serverProtos {
    for _, c := range clientProtos {
        if s == c {
            return s, nil
        }
```

实测（自签证书 + `tls.Listen`/`tls.Dial`，Go 1.27.2）：

```
server[/1,/2] client[/2]      -> r/2
server[/2,/1] client[/2]      -> r/2
server[/2,/1] client[/2,/1]   -> r/2
server[/1]    client[/2,/1]   -> r/1
server[/1]    client[/2]      -> 握手失败：no application protocol
server[/2,/1] client[/1]      -> r/1
```

**两个结论：**

1. **服务端列表的顺序决定一切**：服务端 `[/1, /2]` + 客户端 `[/2]` → 协商到 `/2`。
   所以**锚点必须把新的 ALPN 放在前面**，否则会协商到老 ALPN。
2. **客户端必须同时广告新旧两个**，否则连老锚点时握手直接失败
   （`no application protocol`）。这条实测直接否掉了「客户端只广告新 ALPN」的
   做法。

**复现方法**（自签证书 + `tls.Listen`/`tls.Dial`，不依赖本仓库的任何东西）：

```go
// 服务端 tls.Config{NextProtos: serverProtos}，客户端
// tls.Config{NextProtos: clientProtos, InsecureSkipVerify: true}，
// 握手后读 conn.ConnectionState().NegotiatedProtocol。
// 完整脚本见本节开头的实测输出对应的 6 组参数。
```

**注意**：这个实验用的是 `crypto/tls` 的 TCP 路径，而本仓库走的是 **QUIC**。
QUIC 的 ALPN 由 TLS 1.3 的扩展 16 承载，**协商规则相同**（同一个
`negotiateALPN`），但**落地前仍应在真实 QUIC 连接上复跑一遍**——这是第二步的
验收项，不属于第一步。

### 4.4 三个可选方案

| 方案 | 做法 | 优点 | 代价 |
|---|---|---|---|
| **① 新 ALPN** | 加 `container-mesh-relay/2`，两端都广告它 | 老端自动落到老 ALPN，**天然双向兼容**；不需要嗅探 | 连接的两端必须都是新版才能用新格式；**新成员 + 老锚点仍然断**（A 情形无解） |
| **② 记录内协商** | 第一条记录带版本；收到不认识的版本就降级 | 一条连接里可以混 | 要在锚点加状态机；老端不会降级（它只会报错），所以还是断 |
| **③ 锚点向下兼容** | 新锚点读记录时**先看首字节**：`{` 就是 JSON，`0xC7` 就是二进制 | **老成员 + 新锚点能通**；新锚点同时是两边的中转 | 新锚点的转发路径要按格式分叉；**老锚点 + 新成员仍然断** |

### 4.5 推荐：① + ③ 组合

**锚点（服务端）**：`NextProtos = ["/2", "/1"]`（**新的在前**，§4.3 实测 1）。
**成员（客户端）**：`NextProtos = ["/2", "/1"]`（**两个都广告**，§4.3 实测 2）。

| 连接 | 协商结果 | 记录格式 |
|---|---|---|
| 新成员 → 新锚点 | `/2` | 二进制 |
| 新成员 → 老锚点 | `/1`（老锚点只广告 `/1`） | **JSON 降级** |
| 老成员 → 新锚点 | `/1`（老成员只广告 `/1`） | **JSON 降级** |
| 老成员 → 老锚点 | `/1` | JSON |

**但这里有一个必须正视的缺口：两条腿必须同格式。**

新成员 A 连的是新锚点 B（`/2`，二进制），而 B 连目标 C 时，若 C 是老成员，
B↔C 只能是 `/1`（JSON）。**此时 A 说二进制、C 说 JSON，锚点在中间没法逐字节
转发。**

**这正是方案 ③ 必须存在的原因**：新锚点**必须能同时说两种格式**，并且知道
**每条腿**用哪种：

- 锚点从腿 1 读到二进制帧，转成 JSON 写进腿 2（或者反过来）。
- 转换**只在两条腿格式不同时**才发生——**两条腿都是 `/2` 时才是纯逐字节转发**，
  那才是 8,509× 的那个收益。

所以第二步的锚点路径是：

```
读头 → 若本腿格式 == 对腿格式：原样转发（快路径）
             ↓ 否则
       解码 → 重新编码成对腿的格式（慢路径，仅混合版本时）
```

**这意味着「两条腿都是新版」是收益的前提**，而「两条腿格式一致」是正确性的前提。
第二步的最小改动清单（§5）必须包含**每条腿的格式状态**。

- **情形 A（老锚点 + 新成员）仍然无解**：老锚点的代码写死了 JSON，老锚点不会认识
  新格式。**唯一的解法是部署顺序：先升锚点，再升成员。** 这一条必须写进运维文档。

### 为什么不用「同一 ALPN，两端各自嗅探」

因为老端不会嗅探。老端的 `readRelayRecord` 只有一条路：JSON。所以任何依赖老端
配合的方案都救不了情形 A。**ALPN 是唯一在老端**已经实现**的版本协商机制**——
它在 `direct.go:471` 和 `relaytrunk.go:2643` 已经在用了。

---

## 5. 第二步翻转调用点的最小改动清单

**当前这一步不动任何生产调用点。** 下一步要做的事，按文件列：

### 5.1 `internal/mesh/relaytrunk.go`

| 位置 | 现状 | 改成 |
|---|---|---|
| `relayRecord` (565) | JSON 信封 | **保留**（它仍是内存里的结构），只是不再直接 marshal |
| `writeRelayRecord` (599) | `json.Marshal` + `writeFrame` | `EncodeBinaryRelayFrame` + `writeFrame` |
| `framedRecordReader.nextRecord` (631) | `readFrame` + `json.Unmarshal` | `readFrame` + `DecodeBinaryRelayFrame` |
| `relayFlow` (729) | `req`/`tgt`/`reqBr`/`tgtBr` | **加每条腿的格式状态**（`reqFormat`/`tgtFormat`），见 §4.5 |
| `forwardRecords` (1212) | 读记录 → 判断 type → 写记录 | **改为逐字节转发**：读 24 字节头 → 按 `PayloadLen` 读剩余 → 原样写出去；**仅当两腿格式不同时**才解码再编码 |
| `maxRelayPlaintext` (521) | `maxRelayRecord*3/4 - 1024` | 可以提到接近 `maxRelayRecord - 24`（少 base64 膨胀） |
| `RelayALPN` (2442) | `container-mesh-relay/1` | 加 `container-mesh-relay/2`；**锚点广告 `["/2","/1"]`，新的在前**（§4.3 实测 1） |

**`forwardRecords` 的新形状**（这是收益最大的那一处）：

```go
// 今天：
rec, err := readRelayRecord(br)      // 解码整条记录（含 base64 解码）
... allowBytes(flow, len(rec.Data))   // 看载荷长度
writeRelayRecord(to, rec)             // 重新编码（含 base64 编码）

// 之后（快路径：两腿格式相同）：
hdr, err := peekHeader(br)            // 只读 24 字节
... allowBytes(flow, hdr.PayloadLen)  // 长度从头里读出来
writeRaw(br, to)                      // 原样转发剩下的字节

// 之后（慢路径：两腿格式不同，仅混合版本时）：
rec, err := readRecord(br)            // 解码
writeRecord(to, otherFormat, rec)     // 重新编码成对腿的格式
```

**注意**：`forwardRecords` 现在还要看 `rec.Type == FrameMeshRelayClose` 来判断
"这一方向结束了"。二进制头里有 `type`，所以**这个判断不用解载荷**——读头就够了。

`allowBytes(flow, len(rec.Data))` 计的是**载荷字节数**。二进制头里 `PayloadLen` 就是
它，**数值完全一致**（封签在记录之上，锚点数的和今天一样）。**配额语义不变。**

**每条腿的格式从哪来？** 从**该腿连接的协商 ALPN**。但 `Session` 接口
（`session.go:53`）**不暴露协商结果**——它只有 Read/Write/Close/deadline。
`quicSession`（`session.go:105`）持有 `*quic.Conn`，从它能拿到
`conn.ConnectionState().TLS.NegotiatedProtocol`。所以第二步要么：

- 给 `Session` 加一个**可选接口**（如 `type alpnSession interface{ ALPN() string }`），
  `quicSession` 实现它；**或**
- 在 `dialTarget` / `DialRelaySession` 那里就把格式**记在 `relayFlow` 上**（推荐：
  格式是**连接**的属性，而 `relayFlow` 本来就持有两条腿的会话）。

**注意 joiner 那条腿**：纯出站成员是通过 `--relay-trunk` **加入**锚点的
（`JoinRelayTrunk`，`relaytrunk.go:2778`），它走的是**同一条 relay ALPN**，所以
方案一致。但 joiner 的连接是**锚点发起的**（`dialTarget` → `t.joiners.Open` →
`DialRelaySession`），格式状态同样来自那条连接的协商结果。

### 5.2 `internal/mesh/direct.go`

- `DialRelaySession` (1516) 的 `tlsCfg.NextProtos = []string{RelayALPN}` →
  `[]string{RelayALPN2, RelayALPN}`（**两个都广告，新的在前**；只广告新的会
  导致连老锚点时握手直接失败——§4.3 实测 2）。
- `Listener.handle` (575) 按协商到的 ALPN 分派 → 需要认 `/2`。
- `Listener` 的 `tlsCfg.NextProtos` (471) → 广告 `[/2, /1]`，**新的在前**
  （§4.3 实测 1：服务端列表顺序决定协商结果）。

### 5.3 `internal/mesh/route.go` / `internal/cli/`

- 无需改（格式在 relay 记录层内闭合）。
- **但** `--relay-trunk` 的成员端需要能降级发 JSON（见 §4）。

### 5.4 测试

- `internal/mesh/relaytrunk_*_test.go` 里直接 `json.Marshal(relayRecord{...})` 的地方
  （例如 `relaytrunk_bigfile_test.go:471`）要改用新编码器。
- 需要新增：**混合格式的 e2e 测试**（新锚点 + 老成员、老锚点 + 新成员），断言
  失败是响亮的。

---

## 6. 为什么 E2E transcript 不受影响

**因为封签在记录之上，不在记录之内。**

实测：锚点的 CPU profile 里**没有** `E2ESession.Seal` / `E2ESession.Open`。锚点从
来不封签也不解签——它只搬字节。

- **E2E transcript**（`session.go:240` 的 `e2eTranscript`）覆盖的是：
  domain 串 `"cmesh-e2e-v1"`、transcript 版本字节 `0x01`、临时公钥、双方 DeviceID、
  session id、时间戳。**这六项没有一个来自记录信封。**
- session id 来自 `E2ESession.SessionID()`（`session.go:364`），它是 `E2ESession` 的
  字段，记录信封的 `Session` 字段**从来没被 E2E 用过**。
- 临时公钥来自 `E2ESession.Hello()`（`session.go:377`），也不来自记录信封。
- 记录里的 `hello` 字段是 `E2EHello` 的 JSON 编码（`EncodeE2EHello`，
  `session.go:266`）——**它保持 JSON 不变**（§1）。

所以：**改记录信封不改变 transcript 的任何一个字节。** 签名照样验证得过，派生出的
会话密钥一模一样。

**唯一要盯的是 `hello` 字段**：它现在是 JSON，第二步必须**继续是 JSON**。如果谁
「顺手」把 hello 也改成二进制，`DecodeE2EHello`（`session.go:270`）就会失败——
但那不会破坏 E2E 的**安全性**（签名不成立就是拒绝），只会**破坏可用性**（握手失败）。
`TestRelayFrameHelloIsCarried` 断言 hello 仍是合法 JSON。

---

## 7. 「第一步是否足够」的判断

**判断：不够，但比原计划更接近。**

### 7.1 「只要让 `forwardRecords` 逐字节转发，不用完整的二进制格式」——这个方案不成立

考虑「保留 JSON 体，只改外层」的做法，让锚点转发时不解析 JSON：

1. **锚点转发的是什么？** 如果它原样转发 JSON 文本，下游成员收到的是
   `{"t":3,"data":"SEVM..."}`，成员必须自己 base64 解码才能拿到密文。**密文的表示
   形式从「字节」变成了「文本」**，成员端的解码负担只是**从锚点搬到了成员**——
   总 CPU 没省，还多了一次 base64 编码/解码的往返。
2. **锚点怎么知道载荷多长？** 要给 JSON 体加长度前缀。而 JSON 体自己的长度是
   base64 之后才知道的——所以「定长头 + JSON 体」的记录，**头里读不出载荷长度**，
   除非锚点**扫一遍 JSON 体去找 base64 字段**（还是要解析）。
3. **实测**：§3.3 的 `JSON/verbatim+base64` 一行显示，「复制 JSON + 取回密文」是
   **88,193 ns/op**，与今天完整的 JSON 转发（106,975 ns/op）只差 17.6%；而完整二进制
   是 **12.57 ns/op**。**差 7,015 倍。**

**结论：base64 必须在锚点这一侧就被拆掉，否则「逐字节转发」转的是 base64 文本，
省不下 CPU，只是把成本搬家。** 完整的二进制格式不是过度设计，是这个问题的必要条件。

### 7.2 「第一步」本身够不够？

**第一步（只有格式、无调用点）当然解决不了问题——它没有改变任何线上行为。**
它解决的是：

- 把格式**定死**（tag、版本、头布局、字段顺序、出现规则），使第二步是**机械替换**
  而不是**重新设计**；
- 把两个格式的**等价性**用测试钉死（`TestRelayFrameCrossFormatAgreement`），
  使第二步可以放心地**换掉** JSON；
- 提供**可比基线**（本文件的全部基准数字），使第二步的收益**可验证**而不是
  「应该会更快」；
- 让**格式 tag 的兼容性性质**在切换前就被证明（老解析器必然响亮失败）。

### 7.3 真正的问题在第二步，而且**不在格式里**

**本设计里唯一还没解决的真问题是版本协商**，而它有两个维度：

1. **连接维度（ALPN）**：格式的 tag 让失败**响亮**，ALPN 的版本让失败**不发生**。
   §4.3 的实测给出了 Go 的 ALPN 协商规则（服务端优先），§4.5 的 ①+③ 组合是推荐
   方案。
2. **路径维度（两条腿）**（§4.0）：锚点在中间，A↔C 的记录格式必须一致，否则
   锚点只能转换而不能转发。**这意味着收益的前提是「两条腿都是新版」**——
   这是一条**部署顺序要求**，比格式本身更要紧。

**这两条都不是第一步能解决的，因为它们都需要动生产调用点。** 第一步能做的是把
格式定死、把基线量出来、把失败性质证明清楚——这正是它做的。

### 7.4 一句话结论

**完整的二进制格式是必要的（§7.1 有实测证据：省不掉 base64 就省不掉 CPU，
只省 17.6%）；但它只解决了一半——另一半是 ALPN 版本协商和「两条腿同格式」的
部署约束，这两步都属于第二步。**

---

## 8. 附：本轮新增的文件

| 文件 | 行数 | 内容 |
|---|---|---|
| `internal/mesh/relayframe.go` | 512 | 格式定义 + 编解码 + `PeekRelayFrame` / `SplitRelayFrame` |
| `internal/mesh/relayframe_test.go` | 1232 | 25 个测试 + 4 个基准（10 个子基准） |
| `docs/design/RELAY-FRAME.md` | 本文 | 设计稿 |

**没有改动任何生产调用点。** `relaytrunk.go` / `direct.go` / `session.go` /
`internal/cli/` / `internal/discovery/` / `packaging/` 一行未动。
