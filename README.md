# container-mesh (cmesh)

**一张跟着你的容器到处走的私有局域网。**

把散落各处的机器——云服务器、家里的电脑、公司的容器、没有公网 IP 的受限沙箱——
组成一张私有 mesh。装上之后设备之间互相发现、互传文件、互发消息、互测延迟，
如同插进同一台交换机：

- **零 root**：纯用户态，不碰 TUN，不需要 `CAP_NET_ADMIN`
- **零入站端口**：客户端只出站连到发现节点，不需要安全组、不需要端口映射
- **零第三方**：没有云端注册、没有 CA、没有账号中心；身份就是设备自己的密钥

```
   容器 A ─┐                        ┌─ 容器 D
   容器 B ─┼─ 发现节点(锚点) ───────┼─ 容器 E
   容器 C ─┘   名册 + 存活 + 端点    └─ 容器 F
        └──────── 点对点 QUIC 直连 / 中继兜底 ────────┘
```

---

## 1. 安装

发布页提供各平台静态二进制与 SHA256SUMS 校验清单。安装三步（下载 → 校验 → 落盘）：

```bash
# ① 下载（按你的系统与架构选一个；linux-arm64 / darwin-arm64 / windows-amd64 同理）
curl -fsSLO https://github.com/muzimu217/container-mesh/releases/latest/download/cmesh-latest-linux-amd64
curl -fsSLO https://github.com/muzimu217/container-mesh/releases/latest/download/SHA256SUMS

# ② 校验完整性——不匹配就不要装
sha256sum -c SHA256SUMS --ignore-missing

# ③ 安装到 PATH 上的目录
install -m 0755 cmesh-latest-linux-amd64 ~/.local/bin/cmesh
```

也可以自行构建（无 CGO、单静态二进制）：

```bash
go build -o cmesh ./cmd/cm
```

发布件覆盖 linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64；
组装命令见 `tools/mkrelease`（`go run ./tools/mkrelease --in dist/bin --out dist/release --version v1.0.0`），
发布时每个平台另存一份 `cmesh-latest-<os>-<arch>` 别名，上面的默认地址即可直取。

## 2. 三步组网

```bash
# ① 锚点机器（通常是那台有公网 IP 的服务器）
cmesh init                 # 生成本机身份（私钥加密存储在本机）
cmesh account create       # 生成团队账号密钥（只放最信任的机器上）
cmesh account device-add   # 把本机签进账号
cmesh serve-discovery --addr 0.0.0.0:18090 --announce <公网IP>:18090 --panel

# ② 另一台机器（容器、受限沙箱都行）
cmesh init
cmesh account authorize --pub <对端公钥>     # 由账号持有方签发
cmesh join <invite> --listen 0.0.0.0:18090

# ③ 面板（可选，但这是产品最直观的一面）
#    启动时终端会打印面板地址与令牌，浏览器打开后粘贴令牌即可
```

`cmesh invite --disc <发现节点>` 打印加入串；`cmesh join <invite>` 加入并常驻，
持续打印实时成员视图。

## 3. 网里能做什么

| 动作 | 命令 | 说明 |
|---|---|---|
| 看全网 | `cmesh serve-discovery --panel` | 面板显示在线数、名册、端点；离线设备**保留在名册里**，不除名 |
| 测延迟 | `cmesh mesh ping <deviceID>` | 自定义 RTT 测量（非 ICMP），直连 p95 < 5 ms |
| 发消息 | `cmesh msg <deviceID> --text "…"` | 对端常驻进程实时打印 |
| 传文件 | `cmesh send-to <deviceID> --file PATH` | 直连流式传输，sha256 校验；目录按 tar 打包，权限逐位保留 |
| 远程诊断 | `cmesh run <deviceID> @info` | 内置只读动词（`@info/@disk/@mem/@cat/@ls`），退出码透传 |

> 任意命令执行尚未开放（需要权限分层 ACL），当前只有内置只读动词可用。

## 4. 现在的状态

**已实测通过**（多环境交叉验证）：

- 端到端组网：三台设备互相可见、互拨、互传（100 MB 实测 72.3 MB/s）
- 审批 → 可拨：≤5 秒（长待批场景原为 26 秒，已修复）
- 成员离线/上线可见性：秒级收敛（原最长 147.7 秒，已修复）
- 直连 RTT：同机房 p95 0.63–2.13 ms
- 断线重连、断点续传、目录传输权限语义、限流退避：均已闭环
- 跨公网真实组网：Mac（NAT 后）↔ 云服务器（锚点）实测通过

**明确未做到**（不藏着）：

- 中继路由（R-3.2/R-3.3）与增量视图：**尚未实现**，S3 全受限场景暂靠人工转发兜底
- 统一凭证一键入网：**基础包已落地**（凭证格式、委派签发、额度与有效期），发现节点的准入接线与客户端命令**下一步**
- 桌面 / 手机 App：方案已定（[APP-PACKAGING.md](docs/architecture/APP-PACKAGING.md)），**尚无安装包**
- 图形界面（面板）为轻量只读版；完整可视化按已验收的原型重建

## 5. 文档

| 你想… | 读 |
|---|---|
| 知道这是不是你要的产品 | [REQUIREMENTS.md](docs/REQUIREMENTS.md) |
| 了解四层架构 | [DESIGN.md](docs/DESIGN.md) |
| 部署一台锚点 | [deploy/MESH-NET.md](deploy/MESH-NET.md) |
| 跨平台 App 方案 | [docs/architecture/APP-PACKAGING.md](docs/architecture/APP-PACKAGING.md) |
| M4 交付计划 | [docs/M4-PLAN.md](docs/M4-PLAN.md) |
| 技术决策历史（含被否决的方案） | [docs/decisions.md](docs/decisions.md) |
| 每轮实测报告 | [docs/ROUND-LOG.md](docs/ROUND-LOG.md) |

## 6. 安全边界（诚实版）

- 身份即设备密钥：Ed25519 密钥就是 TLS 证书，无 CA、无主机名校验
- 发现节点**受信任在线，但不被信任诚实**：它可以拒绝服务，但不能伪造成员
  （证书由账号私钥签发，客户端逐个校验）
- 成员视图里的「在线」「有端点」「已拨号成功」是三件不同的事，UI 不会混为一谈
- 加密分两层：每跳会话 TLS + 端到端签名帧，中转节点看不到内容
- 面板的数据接口需要令牌；**未配置令牌的节点根本不会挂载面板路由**

## 7. 许可

AGPL-3.0-or-later。见 [LICENSE](LICENSE)。
