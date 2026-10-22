# container-mesh Demo 包 — 即开即用指南

这个包让你在一台**只有出站网络**的云主机/容器里，几分钟内跑起一台中继（relay），
然后在你本地和这台机器之间传文件、传消息、转发端口。

不需要：公网入站端口、`CAP_NET_ADMIN`、TUN 设备、root、任何依赖。

---

## 一、包里有什么

```
cmesh-demo/
├── cmesh-linux-amd64         # x86_64 服务器
├── cmesh-linux-arm64         # ARM64 服务器（含云厂商的 ARM 实例）
├── cmesh-darwin-arm64        # Apple Silicon Mac（M1/M2/M3…）
├── cmesh-darwin-amd64        # Intel Mac
├── cmesh-windows-amd64.exe   # Windows 64 位
├── start.sh                  # 一键起中继（Linux）
├── stop.sh                   # 停掉后台中继
└── README.md                 # 本文件
```

**同一个包同时覆盖服务端和桌面端。** 全部是静态二进制，不需要装 Go、不需要装运行库。

| 你的机器 | 用哪个 | 备注 |
|---|---|---|
| Linux 服务器 | `cmesh-linux-amd64` / `-arm64` | `start.sh` 会自动选 |
| Mac（Apple Silicon） | `cmesh-darwin-arm64` | 直接在终端跑 |
| Mac（Intel） | `cmesh-darwin-amd64` | |
| Windows | `cmesh-windows-amd64.exe` | 在 PowerShell 或 cmd 里跑 |

> **macOS 首次运行**会被 Gatekeeper 拦一次（未签名）。解决办法：
> `xattr -d com.apple.quarantine cmesh-darwin-arm64`，或在「系统设置 → 隐私与安全性」里点「仍要打开」。

`start.sh` 只在 Linux 上有意义（它是给服务器一键起中继用的）；桌面端直接调二进制即可。

---

## 二、三步跑起来

### 1. 上传并解压

在你本地执行（把 `USER@HOST` 换成你的服务器）：

```bash
scp cmesh-demo.tar.gz USER@HOST:/tmp/
ssh USER@HOST
cd /tmp && tar xzf cmesh-demo.tar.gz && cd cmesh-demo
```

### 2. 启动中继

```bash
./start.sh -d
```

输出会告诉你监听地址和**对端该用的 `--relay` 参数**，例如：

```
container-mesh relay
  listening   :8080
  version     c9686d5

  From another machine, connect with:
      --relay ws://203.0.113.10:8080/ws
```

记下那一行 `--relay`。

> **后台运行**：`-d` 会把中继放到后台，日志写到 `./relay.log`。
> 想直接前台看日志就去掉 `-d`。

### 3. 确认它活着

```bash
./cmesh-linux-amd64 version
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/ws
```

`/ws` 直接 HTTP GET 会返回 400（它只接受 WebSocket 升级），**这不是错误**，
说明端口在监听。

---

## 三、从你本地连过去

本地也需要一个 `cmesh`。三条路，选一条：

### A. 用同一个包里的二进制（推荐）

包里已经包含你本地要用的那个：

```bash
# macOS（Apple Silicon）
chmod +x cmesh-darwin-arm64
xattr -d com.apple.quarantine cmesh-darwin-arm64 2>/dev/null || true
./cmesh-darwin-arm64 version

# macOS（Intel）
chmod +x cmesh-darwin-amd64

# Windows（PowerShell）
.\cmesh-windows-amd64.exe version
```

不需要装 Go，也不需要 Docker。

### B. 传文件

```bash
# 接收端（先起，等你发）
./cmesh recv --relay ws://203.0.113.10:8080/ws --code demo1234 --out ./inbox

# 发送端（另开一个终端）
./cmesh send --relay ws://203.0.113.10:8080/ws --code demo1234 --file ./some.bin
```

两端会打印同一个 **SAS**（6 位数字）。**对一下**，不一致就是有人中间人。

传完两端都会校验 SHA-256。大文件中断会自动续传。

### C. 传目录

`--file` 直接给目录就行，会打包传输、对端解包：

```bash
./cmesh send --relay ws://203.0.113.10:8080/ws --code demo1234 --file ./myproject
```

### D. 端口转发（阶段 1 新功能）

假设云主机上跑着一个只在它本地可达的服务（比如 `127.0.0.1:5432` 的数据库），
你想从本地连它：

**云主机侧**（允许对方拨我配置的地址）：

```bash
./cmesh-linux-amd64 forward --relay ws://203.0.113.10:8080/ws --code demo1234 \
    --accept --allow-host 127.0.0.1 --allow-port 5432
```

**你本地**（把本地 15432 映射过去）：

```bash
./cmesh forward --relay ws://203.0.113.10:8080/ws --code demo1234 \
    -L 127.0.0.1:15432:127.0.0.1:5432
```

然后 `psql -h 127.0.0.1 -p 15432` 就连上了。

反向也行（让对方监听端口，回连到你本地）：

```bash
# 你本地：请求对方监听 9000，映射回我这里的 3000
./cmesh forward --relay ws://203.0.113.10:8080/ws --code demo1234 \
    -R 127.0.0.1:9000:127.0.0.1:3000 --accept-reverse

# 云主机：托管那个监听器
./cmesh-linux-amd64 forward --relay ws://203.0.113.10:8080/ws --code demo1234 \
    --accept-reverse --allow-listen-port 9000
```

---

## 四、网络与防火墙

中继只需要**一个入站 TCP 端口**（默认 8080）。客户端**只做出站连接**。

如果云厂商有安全组/防火墙，放行该端口：

```bash
# 有 firewalld 的机器
sudo firewall-cmd --add-port=8080/tcp --permanent && firewall-cmd --reload

# 有 ufw 的机器
sudo ufw allow 8080/tcp
```

### 如果安全组改不了（很常见）

云服务器的入站端口常常卡在**厂商安全组**，系统内的 iptables 是 ACCEPT 也没用。
这种情况下不要把时间花在安全组上，直接用**已有的 80/443 走反向代理**——这也是
更接近生产的做法。

在已经有 nginx 的机器上，**加到现有 server 块里**（不要新建 server 块：
同一个 `server_name` 的第二个 server 块会遮蔽原有站点，这是踩过的坑）：

```nginx
# 在已有的 server { ... } 里加一个 location
location /cmesh/ {
    proxy_pass http://127.0.0.1:8082/ws;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
}
```

然后：

```bash
sudo nginx -t && sudo systemctl reload nginx
curl -s -o /dev/null -w '%{http_code}\n' http://你的域名/cmesh/    # 期望 400
```

客户端的 `--relay` 用 `ws://你的域名/cmesh/`（或 `wss://`，如果那个 server 块有 TLS）。

> `400` 是**正确的**：该路径只接受 WebSocket 升级，普通 HTTP GET 会被拒。
> 如果拿到 `502`，说明 nginx 连不上中继——检查 `start.sh -d -p` 用的端口是否和
> `proxy_pass` 一致。

### 换端口

```bash
./start.sh -d -p 9000
```

### 走 TLS（wss://）

有域名和证书时，中继可以直接终止 TLS：

```bash
./start.sh -d --tls /path/fullchain.pem /path/privkey.pem
```

它会打印 `--relay wss://...`。没有证书就用 `ws://`（仅建议在内网或临时 Demo 用）。

---

## 五、Demo 前自检清单

按顺序做一遍，全过就说明可以演示了：

| # | 检查 | 命令 | 期望 |
|---|---|---|---|
| 1 | 二进制能跑 | `./cmesh-linux-amd64 version` | 打印版本号 |
| 2 | 中继在监听 | `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/ws` | `400` |
| 3 | 本机自环传消息 | `./cmesh-linux-amd64 send --relay ws://127.0.0.1:8080/ws --code demo1234 --message hi`（另开终端先起 `recv`） | 收到 `hi` |
| 4 | 本机自环传文件 | 同上但用 `--file` | SHA-256 一致 |
| 5 | 跨机连通 | 从你本地 `send` 到云主机 `recv` | SAS 相同、文件校验通过 |
| 6 | 端口转发 | 第三节 D 的两条命令 | 本地端口能连到远端服务 |

第 3、4 步不需要跨机，是确认中继本身健康的最快方式。

---

## 六、停止与清理

```bash
./stop.sh          # 停掉 -d 启动的中继
rm -rf /tmp/cmesh-demo
```

中继不落盘任何数据：它只转发密文，不记录 code、不写文件。

---

## 七、安全边界（演示时值得说清楚）

- **中继看不到内容**：传输是 SPAKE2 握手 + AES-256-GCM。中继只看到密文。
- **中继看不到 code**：房间号是 code 的 HMAC 派生值。
- **但要防主动中继**：知道 code 的人可以冒充对端。**两端打印的 SAS 必须一致**，
  用 `--expect-sas 123456` 可以强制校验，不一致直接中止。
- **code 要够长**：不给 `--code` 时会自动生成一个强随机码并打印一次。别用 `1234`。

---

## 八、常见问题

**`start.sh` 报 `cannot execute` / `Permission denied`**
所在目录可能是 `noexec` 挂载（常见于 `/tmp` 的某些配置）。把整个目录拷到
`$HOME` 下再试。

**`start.sh` 报 `unsupported architecture`**
只提供 amd64 与 arm64。用 `uname -m` 确认；其他架构需要从源码构建。

**连接一直 `connecting` / `connect-failed`**
1. 确认 `--relay` 的地址和端口对（`start.sh` 打印的那行）
2. 从本地测端口：`nc -zv HOST 8080`
3. 云厂商安全组是否放行
4. 中继日志：`tail -f relay.log`

**两端一直配不上对**
99% 是 `--code` 不一致。房间号由 code 派生，code 不同就永远在两个房间。
用 `--pair-window 8s` 让它快速失败而不是干等。

**但如果你看到 `no partner appeared within 25s` 反复出现，先看是不是给接收端
加了 `--stay=false`。** 一次性模式会把配对窗口压到 25 秒、总共 6 次尝试，
也就是 150 秒预算；对方晚到一点就错过。**接收端去掉 `--stay=false`
（用默认值）在单 relay 下是无限等待的**，这才是「先起接收端等发送端」该用的模式。
确实需要一次性退出时，显式给预算：`--pair-window 4m`（6 × 4m = 24 分钟）。

**端口转发报 `denied`**
看对端日志里的 `reason`：`--accept` 是否开了、`--allow-host` / `--allow-port`
是否包含你要的目标。默认是**拒绝一切**，这是刻意的。
