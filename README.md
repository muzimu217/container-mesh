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
组装命令见 `tools/mkrelease`：

```bash
go run ./tools/mkrelease --in dist/bin --out dist/release --version v1.0.0
#   需要 packaging/install.sh 存在（发布必带安装器）；缺它会明确失败且不留半成品。
#   编译源：先按平台交叉编译出 dist/bin/cmesh-<version>-<os>-<arch>，再组装。
```

产物：各平台二进制 + 同名 `.tar.gz`（内含二进制、installer、LICENSE）+ `install.sh`
+ `SHA256SUMS`（覆盖全部 11 个文件，可用 `shasum -a 256 -c SHA256SUMS` 校验）。

> **`cmesh-latest-<os>-<arch>` 是发布时的别名**：`mkrelease` 除了版本化文件名
> （`cmesh-v1.0.0-linux-amd64`），还会为每个平台产出一份 `cmesh-latest-<os>-<arch>`
> 副本——GitHub 的 `releases/latest/download/<name>` 只认名字里带 `latest` 的资产，
> 有了这个副本，上面的默认地址就能直取。发布时把 `dist/release/` 整个目录上传即可。


## 2. 三步组网

> **旗标与位置参数**：口令类旗标（`--passphrase` / `--account-passphrase` /
> `--device-passphrase`）在非交互环境必填，否则命令会等口令直到 EOF 而失败；
> **位置参数一律放最后**（`cmesh join <invite>` 的邀请串、`msg`/`send-to`/
> `mesh ping` 的设备 ID 都在旗标之后）。

```bash
# ① 锚点机器（通常是那台有公网 IP 的服务器）
cmesh init --passphrase <口令>                       # 生成本机身份（私钥加密存储在本机）
cmesh account create --passphrase <口令>             # 生成团队账号密钥（只放最信任的机器上）
cmesh account device-add --device-passphrase <口令> --account-passphrase <口令>
cmesh serve-discovery --addr 0.0.0.0:18090 --announce <公网IP>:18090 \
    --device-passphrase <口令> --account-pub <账号公钥> --panel
#   --account-pub：让锚点能验证成员证书与团队凭证（`cmesh account show` 打印）
#   --panel：开启可视化面板（终端会打印面板地址与令牌）

# ② 另一台机器（容器、受限沙箱都行）—— 方式 A：团队凭证，一条命令入网
cmesh join --cred <凭证串> --device-passphrase <口令> --listen 0.0.0.0:18090
#   凭证串由锚点机器 `cmesh cred create --disc <锚点IP>:18090 --ttl 24h \
#     --max-devices 10 --account-passphrase <口令>` 打印（一行，cmesh1. 开头）。
#   注意：锚点必须带 --account-pub 启动，否则准入端点不开启（join 会收到 404）。

# ②' 方式 B：逐台审批（邀请串路径）
cmesh init --passphrase <口令>
cmesh show-pub --device-passphrase <口令>            # 把本机公钥给账号持有方
# 账号持有方执行：签发证书后，把证书文件放进锚点的 <state>/certs/ 目录
cmesh account authorize --pub <对端公钥> --account-passphrase <口令>
cmesh join --listen 0.0.0.0:18090 --device-passphrase <口令> <invite-string>
#   邀请串由 `cmesh invite --state <目录> --disc 127.0.0.1:18090 \
#     --account-passphrase <口令>` 打印；它必须放在旗标之后
```

`cmesh join` 加入并常驻，持续打印实时成员视图。

> **状态目录**：以上命令默认用 `~/.cmesh`；想放别处，每个命令都要带 `--state <目录>`
>（`cmesh account show` 用 `--state`，`cmesh invite` 也用——漏一个就会去读默认目录，
> 报「account.json not found」）。

## 3. 网里能做什么

> **设备 ID 放最后**：这些命令的位置参数是设备 ID，写在旗标前面会被当成
> 未知旗标（`rc=2`）。

| 动作 | 命令 | 说明 |
|---|---|---|
| 看全网 | `cmesh serve-discovery --panel` | 驾驶舱面板（3D 名册视图、成员名册、四维状态核验）；离线设备**保留在名册里**，不除名。极简版在 `/panel-lite` |
| 测延迟 | `cmesh mesh ping --device-passphrase <口令> <deviceID>` | 自定义 RTT 测量（非 ICMP） |
| 发消息 | `cmesh msg --device-passphrase <口令> --text "…" <deviceID>` | 对端常驻进程实时打印 |
| 传文件 | `cmesh send-to --device-passphrase <口令> --file PATH <deviceID>` | 直连流式传输，sha256 校验；**仅常规文件** |
| 传目录 | `cmesh send --relay ws://<锚点>:18090/ws --code <配对码> --file DIR` / `cmesh recv --relay ws://<锚点>:18090/ws --code <配对码> --out DIR` | 走 relay 配对通道：目录按 tar 打包，权限逐位保留（0700 不会被放宽成 0755） |
| 远程诊断 | `cmesh run <deviceID> @info` | 内置只读动词（`@info/@disk/@mem/@cat/@ls`），退出码透传 |

> 任意命令执行尚未开放（需要权限分层 ACL），当前只有内置只读动词可用。

## 4. 现在的状态

**已实测通过**（多环境交叉验证；每条都可在 `docs/ROUND-LOG.md` 查到记录）：

- 端到端组网：三台设备互相可见、互拨、互传（100 MB 实测 72.3 MB/s）
- 成员离线/上线可见性：秒级收敛（实测下线 0.01–1.12s、恢复 0.25–3.09s，含慢消费者形态）
- 面板：401/200 鉴权正确、页面 14,699 B 零外部引用、上下线跟随秒级
- 目录传输权限语义：0755/0750/0700/0600/0400 与目录模式逐位保留
- 429 限流退避、握手错误文案、面板令牌鉴权：均已闭环

**明确未做到**（不藏着）：

- 中继路由（R-3.2/R-3.3）与增量视图：**尚未实现**，S3 全受限场景暂靠人工转发兜底
- 组网内的**断点续传**仍待实现（`docs/REQUIREMENTS.md` R-4.2 记录为「待 PR-M5」）；断点续传目前只在 relay 配对通道可用
- 桌面 / 手机 App：方案已定（[APP-PACKAGING.md](docs/architecture/APP-PACKAGING.md)），**尚无安装包**
- 图形界面为**驾驶舱**（按已验收原型实现，3D 名册视图）+ 轻量版 `/panel-lite`

**关于「零入站端口」**：这句话描述的是**客户端**：成员设备只出站连到发现节点，
不需要为入网开任何端口。锚点本身要开一个端口（发现与成员流走 TCP，直连走同一
端口号的 UDP），这是它作为入网点的代价——不开放锚点端口的网络只能靠他人中转。


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
