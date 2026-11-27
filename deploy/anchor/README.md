# deploy/anchor：锚点重装后的一键重建

**主机重装之后，把发现锚点重新立起来应该是一条命令，而不是一次调查。**

## 为什么存在

锚点是全网依赖的那一台：它持有**账号私钥**（`account.json`）、**名册**
（`members.json`）、**设备证书**和**面板令牌**。它的主机一旦重装，这些东西
全部消失——Round 98 那次事故把文件系统的根删了，Round 102 那次重装把整个协调
环境重建了一遍，每一次都是一页笔记加一轮摸索（用户是谁、令牌在哪个文件、unit
到底叫什么）。

这个目录把那套步骤固化成脚本：**`deploy/anchor/install.sh` 在本机交叉编译并
推送，`deploy/anchor/host.sh` 在目标机上以 root 幂等地重建。**

- `systemd/cmesh-serve-discovery.service` —— 目标机上的 **unit 原文**（与真机上
  的那份逐字节一致，已核对）。
- `host.sh` —— 目标机上的负载脚本（幂等）。
- `install.sh` —— 本机这端的包装：编译 → 传输 → 执行 → **读回证据**。
- `host_test.sh` —— 离线回归：把 `host.sh` 连跑两遍，断言第二遍什么都没动。

## 它做什么

1. 建 `cmesh` 用户与组（已存在就跳过）。
2. 建状态目录 `/var/lib/cmesh`（`cmesh:cmesh` 0750）与 `/etc/cmesh`
   （`root:cmesh` 0750）。
3. 装二进制到 `/usr/local/bin/cmesh`：先写同目录下的暂存文件再 `mv`；旧二进制
   改名留作 `cmesh.bak.<epoch>`；**内容相同就不替换**。
4. 装 `/etc/cmesh/discovery.env`（`root:cmesh` 0600——见下面的「布局」）。
5. **身份只在缺失时创建**：没有 `account.json` 才跑 `cmesh init` →
   `cmesh account create` → `cmesh account device-add`。有则一个字节都不碰。
6. 把 `CMESH_ACCOUNT_PUB` 对齐到磁盘上的账号公钥（规则见下面「脚本的规则，
   不是产品的规则」）。
7. 面板令牌文件：已存在就不动；不存在才生成随机值（`root:cmesh` 0640），
   **只打印路径，绝不打印令牌本身**。
8. 装 unit、`daemon-reload`、`enable --now`——**只有机器上真的有 systemd
   （`/run/systemd/system` 存在）才做**，否则打印「跳过」并继续。

### 布局（与真机一致）

| 路径 | 属主:组 | 模式 |
|---|---|---|
| `/usr/local/bin/cmesh` | `root:root` | 0755 |
| `/var/lib/cmesh` | `cmesh:cmesh` | 0750 |
| `/var/lib/cmesh/{account,device,members}.json`、`<deviceID>.cert.json`、`certs/`、`inbox/` | `cmesh:cmesh` | 0600 / 0700 |
| `/etc/cmesh` | `root:cmesh` | 0750 |
| `/etc/cmesh/discovery.env` | `root:cmesh` | **0600** |
| `/etc/cmesh/panel-token` | `root:cmesh` | 0640 |
| `/etc/systemd/system/cmesh-serve-discovery.service` | `root:root` | 0644 |

**关于 `discovery.env` 的模式：这里取 0600，是照真机事实走的。**
`packaging/systemd/README.md` 里写的是 0640。两者 systemd 都能读——systemd 以
root 读 `EnvironmentFile=`，所以 0600 不会挡住 unit。区别只在于「除了 root 之外
还有谁能读它」：0600 只有 root 能读，而这份文件里装着 `CMESH_PASSPHRASE`。
脚本按真机现状取 0600；如果你更想要 0640（`cmesh` 组可读），装完之后
`chmod 0640 /etc/cmesh/discovery.env` 即可，脚本不会再去动它（它只在**安装**那
一步设一次模式）。

### 脚本的规则，不是产品的规则

**`CMESH_ACCOUNT_PUB` 的写回规则是脚本自己的判断，不是产品已有的规则**，写在这里
以免被当成产品语义：

- 脚本**刚重建了账号**（`IDENTITY_CREATED=1`）：`CMESH_ACCOUNT_PUB` 与磁盘上的账号
  对不上时，脚本改写它，并留一份 `discovery.env.bak.<epoch>`。
- 账号是**原有的**、只是公钥对不上：脚本**只告警，不改**。理由是磁盘上的账号私钥
  才是那个说了算的东西，脚本没有理由用一个读出来的公钥去覆盖运维的判断。
- 对得上：什么都不做。

产品那边的既有规则是：`--account-pub` 少了或对不上，**入网接口根本不挂载**（节点
看起来一切正常，成员却一个都加不进来，而且没有任何报错）。脚本对齐它是为了不让
这一种静默失败发生在自己手里。

## 它不做什么

- **不编造 `/etc/cmesh/discovery.env`。** 监听地址与对外公告地址是这台机器的
  事实，不是脚本能猜的：文件必须已经在主机上，或者用
  `CMESH_DISCOVERY_ENV_SOURCE` 指一份本地文件交给它安装。脚本只管这份文件的
  **属主与模式**（`root:cmesh` 0600——它里面装着口令）。
- **不打印、不轮换面板令牌。** 已有的令牌文件一个字节都不动。
- **不删除任何东西**，除了它自己创建的、名字固定的暂存文件
  （`cmesh.new`、`panel-token.new`、`discovery.env.new`）。
- **不碰 Hub、不碰成员节点、不碰这台机器上的别的东西。**
- **不把锚点迁到安装器托管布局**（`/opt/container-mesh/current/cmesh`）。那是
  后面里程碑的事；真机上的 unit 现在跑的就是 `/usr/local/bin/cmesh`。
- **不在命令行里传口令。** 见下。

## 口令怎么传给 CLI

`cmesh init` 与 `cmesh account create` 读口令的顺序是
**`--passphrase` 旗标 → `CMESH_PASSPHRASE` 环境变量 → 交互提示**
（`internal/cli/mesh.go` 的 `readPassphrase`）。

旗标会进 `ps` 的输出，所以脚本**不用旗标**：口令从 `discovery.env` 里读出来
（是**读**，不是 `source`——`source` 一个 root 拥有的配置文件等于执行它），
再 `export` 进子进程的环境。`CMESH_PASSPHRASE` 因此只出现在子进程的
`environ` 里，不出现在任何命令行、任何输出里。

脚本在 `host.sh` 里的对应位置：

- 读值：`env_get()`（awk 解析 `KEY=value`，处理引号，不展开变量）；
- 导出：`export CMESH_PASSPHRASE="$PASSPHRASE"`；
- 以服务用户运行：`run_as_service_user()`（见下一节），stdin 关闭，所以它永远不会
  等一个人敲口令。

`account create` 用的是 `--passphrase`（不是 `--account-passphrase`），而
`CMESH_PASSPHRASE` 对两者都生效，所以**设备口令与账号口令是同一个值**。
`account device-add` 需要设备口令与账号口令，也同样由这个环境变量提供。

## ★ 哪条分支在哪台机器上生效

口令必须**穿过**「以服务用户运行」这一步才能到 CLI，而这一步在不同机器上用的
工具不同，**行为也不同**。所以脚本是**先探测、再选择**，不是假设。

| 机器上有什么 | 走哪条分支 | 口令能不能到 CLI |
|---|---|---|
| 有 `runuser` | `runuser -u cmesh -- …` | 能。`runuser` 透传环境 |
| 没有 `runuser`，有 `setpriv` | `setpriv --reuid=cmesh … -- …` | 能。`setpriv` 透传环境 |
| 只有 `su` | **先探测**，再在 `su` / `su -p` / `su -m` 里挑一个**实测能保住环境**的 | 能，但取决于这台机器的 `su` |
| 只有 `su`，而且它的所有变体都重置环境 | **拒绝执行**，打印手工步骤，退出非零 | **不能**——脚本不会硬凑 |

### 为什么 `su` 要单独探测

**`su` 不保证保留环境。** 它的子进程 `environ` 是重建的（至少 PATH、HOME、SHELL、
USER、LOGNAME，有些发行版重建得更彻底），`CMESH_PASSPHRASE` 会被一起丢掉。

丢掉的后果很具体：`readPassphrase` 拿不到环境变量，就去读交互提示，而脚本把
stdin 关成了 `/dev/null` ⇒ **立刻 EOF 失败**。也就是说：一条注定失败的命令。

而这恰恰是最要命的地方——**重装之后遇到老机器的概率最高**，而「在老机器上失败」
正是这个脚本要消灭的那件事。

所以脚本用**探针**决定：每个 `su` 变体都真跑一个探针进程，问它
`CMESH_PASSPHRASE` 还在不在（探针只回答「在/不在」，**不打印口令的值**），
挑第一个能保住环境的变体。实测里 Debian 12 的 `su -p` 就能保住。

**一个变体都不行时，脚本拒绝执行并打印手工步骤**（下面），而不是跑一条注定
失败的命令，**更不会**为了让它跑通而把口令塞进命令行——那会让口令出现在 `ps`
里，与「口令绝不进命令行」这条纪律直接冲突。

### 缺 `runuser`/`setpriv` 时的手工步骤

脚本会原样打印下面这三行（路径按实际值填）：

```bash
export CMESH_PASSPHRASE='<discovery.env 里的口令>'
sudo -u cmesh /usr/local/bin/cmesh init --state /var/lib/cmesh
sudo -u cmesh /usr/local/bin/cmesh account create --state /var/lib/cmesh
sudo -u cmesh /usr/local/bin/cmesh account device-add --state /var/lib/cmesh
```

口令走**环境变量**，不进命令行。跑完再执行一次脚本：它会发现身份已经在了，
一个字节都不碰。

（装上 util-linux 的 `runuser`，或者 shadow 的 `setpriv`，脚本下一次就能自己
做完。）

### 回归怎么覆盖这条分支

`host_test.sh` 有两条专门针对它的断言（**断的是结果，不是日志**）：

- `CMESH_FORCE_SU=1` 强制走 `su` 分支（在有 `runuser`/`setpriv` 的机器上也一样），
  然后断言 **账号私钥真的被建出来了，而且能用口令解开**——用
  `cmesh account show` 读回公钥。读不出来就说明口令没到 CLI。
  只看日志的测试在这里会误判通过：口令没送到，命令也可能看起来跑了。
- 用一个**会丢环境的桩 `su`**（`exec env -i …`）模拟最老的机器，断言脚本
  **非零退出 + 打印了手工步骤 + 没有建出任何身份文件**。半截的身份比没有身份
  更糟，所以这一条也是断言。

## 用法

### 一条命令（本机这端）

```bash
# 目标机上已经有一份 discovery.env（或者从别处拷一份过去）
deploy/anchor/install.sh root@<host>

# 顺便把一份 discovery.env 带过去
CMESH_DISCOVERY_ENV_SOURCE=./discovery.env deploy/anchor/install.sh root@<host>

# 只看它要做什么，不真连
DRY_RUN=1 deploy/anchor/install.sh root@<host>
```

脚本会：交叉编译 `linux/amd64`（`CGO_ENABLED=0`，版本号用 `-ldflags` 打上，
与 `.github/workflows/ci.yml` 同形）→ 传负载、二进制、unit → 在目标机上跑
`host.sh` → **读回 `cmesh version` 与 `systemctl status` 作为证据**。

目标机是 `arm64` 的话：`CMESH_GOARCH=arm64`。

### 只跑负载（目标机上，root）

```bash
CMESH_BIN_SRC=/tmp/cmesh.install \
CMESH_UNIT_SRC=/tmp/cmesh-serve-discovery.service \
CMESH_DISCOVERY_ENV_SOURCE=/tmp/discovery.env \
bash host.sh
```

`host.sh` 的变量（都有默认值，见脚本头部）：

| 变量 | 默认 | 是什么 |
|---|---|---|
| `CMESH_PREFIX` | 空 | 把所有路径挪到这个前缀下面（离线回归用它） |
| `CMESH_STATE_DIR` | `/var/lib/cmesh` | 状态目录 |
| `CMESH_ETC_DIR` | `/etc/cmesh` | 配置目录 |
| `CMESH_BIN_PATH` | `/usr/local/bin/cmesh` | 二进制 |
| `CMESH_UNIT_PATH` | `/etc/systemd/system/cmesh-serve-discovery.service` | unit |
| `CMESH_DISCOVERY_ENV` | `/etc/cmesh/discovery.env` | 环境文件 |
| `CMESH_PANEL_TOKEN_FILE` | `/etc/cmesh/panel-token` | 面板令牌 |
| `CMESH_DISCOVERY_ENV_SOURCE` | 空 | 要安装的本地环境文件 |
| `CMESH_BIN_SRC` | 空 | 要安装的本地二进制 |
| `CMESH_UNIT_SRC` | 本目录下的 unit | 要安装的本地 unit |
| `CMESH_SERVICE_USER` / `_GROUP` | `cmesh` / `cmesh` | 服务账号 |
| `CMESH_SKIP_IDENTITY` | `0` | 设成 `1` 就完全不建身份 |

**每个会写或会删的路径都是显式命名的变量，并且在用之前先解析**——解析结果是
`/` 就拒绝（Round 98 的教训：那次是拿路径**字符串**跟 `/` 比，`//`、`/.`、
`/tmp/..` 全都过去了，最后 `os.RemoveAll("/")` 把整台主机的文件系统根删了）。

## ★ 重装之后的操作顺序

**Hub 先，锚点其次，成员最后。**

- **Hub**（`deploy/hub/`）是协调者与各位 Agent 之间的消息通道，跟产品数据面
  无关。Round 102 重建时它是先立起来的——**协调通道先建**，否则重建过程中连
  报告都递不上去。Hub 的安装脚本在 `deploy/hub/install.sh`，它**不生成新令牌**
  （每一个 Agent 手里都握着同一个令牌，换令牌就得改所有人的环境变量，而 Agent
  那边没法通知）。
- **锚点**（就是本目录）必须在**成员之前**。锚点是入网的入口：成员要向它申请
  凭证、它按名册放行。锚点没起来，成员就无处可去。Round 102 的记录写得很直白：
  「**锚点**（按部署顺序，锚点先、成员后）」。
- **成员最后**：锚点起来之后，全体成员重新入网（见下一节）。

锚点与 Hub 在**不同的机器**上（锚点在有公网 IP 的那台，Hub 在官网服务器上），
所以「Hub 先」指的是协调通道先恢复，不是同一台机器上的先后。

## ★ 身份是最难重建的那一件

主机重装之后，二进制、unit、目录都是**可以重新生成**的：换个版本而已。

**身份不是。**

`account.json` 是账号私钥，它签发设备证书。一份新的账号私钥意味着：

- 旧账号签出去的**每一张设备证书都作废**；
- **全体成员必须重新入网**（名册从空开始）；
- 已经拿到手的老凭证（`cmesh cred create` 出来的那些）也要重新发。

所以 `host.sh` 的规矩是：**状态目录里没有 `account.json`，才建身份；有就一个
字节都不碰**——不重新加密、不 `--force`、不改时间戳。而且建的时候**必须大声
喊出来**：

```
############################################################
# THE ANCHOR IDENTITY WAS REBUILT — EVERY MEMBER MUST JOIN AGAIN
############################################################
```

还有一条容易被忽略的连带关系：**`CMESH_ACCOUNT_PUB` 必须跟着账号走**。unit
把它传给 `serve-discovery`，而少了它（或者它对不上磁盘上的账号）**入网接口根本
不会挂载**——节点看起来一切正常，成员却一个都加不进来，而且没有任何报错。
脚本在重建账号之后会把这个值对齐（并留一份 `.bak.<epoch>`）。

**如果老的 `account.json` 还存在于某个备份里，请把它恢复回去，而不是让脚本
新建一个。** 恢复 = 成员不用重新入网；新建 = 全网重新入网。

## 幂等性

跑第二遍**什么都不该变**。这不是锦上添花：多跑一次 `cmesh init --force` 就是一次
新的账号密钥，全网的证书当场作废，而没人会发现，直到入网开始失败。

`host_test.sh` 断言的就是这件事（在一个一次性的前缀目录里，连跑两遍）：

- 两遍退出码都是 0；
- 身份文件（`account.json` / `device.json` / `<deviceID>.cert.json`）第二遍之后
  的 sha256 与第一遍之后**相同**；
- 二进制内容相同就**不替换**（inode 不变、备份数不变）；
- 旧二进制备份 `cmesh.bak.<epoch>` 存在；
- **输出里没有任何口令或令牌的值**；
- 重建公告只喊一次，第二遍明说「身份已存在，没有动它」；
- `su` 分支两条都覆盖了（成功的那条断的是**账号私钥能用口令解开**这个结果，
  失败的那条断的是**非零退出 + 手工步骤 + 没有建出任何身份文件**）。

## 验证

```bash
# 语法
bash -n deploy/anchor/host.sh deploy/anchor/install.sh deploy/anchor/host_test.sh
shellcheck deploy/anchor/host.sh deploy/anchor/install.sh deploy/anchor/host_test.sh

# 离线回归（需要一个给本机架构的 cmesh 二进制）
CMESH_BIN_SRC=/path/to/cmesh bash deploy/anchor/host_test.sh

# 在容器里（容器没有 systemd，脚本必须能跑完并说清跳过了什么）
docker run --rm -v "$PWD/deploy/anchor:/work:ro" -v /path/to/cmesh:/opt/cmesh.bin:ro \
  debian:bookworm-slim bash -c 'CMESH_BIN_SRC=/opt/cmesh.bin bash /work/host_test.sh'

# unit 的不变量守卫仍然过
go test ./packaging/ -run 'Unit|Invariant' -count=1
```

## 边界

- **真机上的执行还没有做。** 本目录的脚本目前只在本机与容器里验证过；在授权
  主机上跑要等网主放行。跑之前请先 `DRY_RUN=1` 看一遍它要做什么。
- **锚点没有进 `packaging/` 的 unit 不变量守卫表。** 那张表守的是
  `packaging/systemd/` 里随安装器发布的两份 unit，它们的 `ExecStart` 指向托管
  布局（`/opt/container-mesh/current/cmesh`），而真机上这份 unit 执行的是
  `/usr/local/bin/cmesh`——迁移到托管布局是后面里程碑的事，所以这两条断言
  （`TestTheUnitsExecuteTheManagedLayout`、`TestTheUnitsStateTheirLogDestination`）
  对它**不成立**，硬加进去只会让门禁变红（实测过：加进去就是这两条红）。
  **所以这一份 unit 的不变量由 `deploy/anchor/host_test.sh` 守，不在 CI 的守卫
  表里**——CI 现在跑的是 `go test ./packaging/ -run 'Unit|Invariant'`，它守不到
  这份 unit。属于它的不变量是：start limit 在 `[Unit]`、不用 `ExecStartPre`、
  装出来的 unit 与仓库里那份逐字节相同。**`host_test.sh` 目前还没接进 CI**
  （`.github/workflows/` 不在本任务的文件范围内），接线是后续一笔。
- **面板令牌只在文件里。** 脚本不打印它，README 也不写它。要看就去主机上读
  `/etc/cmesh/panel-token`（`root:cmesh` 0640）。
- **组网顺序、端口放行、安全组** 不在脚本范围内：那是主机之外的事。
