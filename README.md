# X-Box — 飞牛 fnOS 系统管理工具箱

单 Go 二进制 + 内嵌 Web UI（**纯 Go 标准库，零第三方依赖**），以 fnOS 原生应用（fpk）形式安装运行，内置 amd64 / arm64 双架构。系统清理、安全体检、DDNS、内网浏览器与 root 终端，一个面板全搞定。

## 功能

| 页面 | 功能 |
|---|---|
| 仪表盘 | CPU（含温度 3s 动态刷新）/内存/负载/运行时长、磁盘用量、监听端口与公网暴露概况、网络信息（网卡/IPv4/IPv6/MAC/网关/DNS/实时速率）、NAT 类型检测（STUN，NAT1–NAT4 等级 + CGNAT 判定 + 优化建议）、存储空间与硬盘温度 |
| 垃圾清理 | 8 类安全分类扫描（apt 缓存、轮转日志、journal、tmp、crash、应用日志/tmp/缩略图），文件级勾选清理 + 全清，白名单 + 年龄阈值双重复核 |
| 端口管理 | 解析 /proc/net 全部 tcp/tcp6/udp/udp6，进程映射、属主、暴露面（公网/局域网/仅本机）分类 |
| 进程与服务 | 全进程 CPU 3s 实时刷新（搜索/排序）、systemd 服务、飞牛应用、Docker 容器一览，支持结束进程 |
| 开机自启 | systemd 服务/定时器、rc.local、cron @reboot、init.d、fnOS 应用启动项扫描 |
| DDNS 服务 | 9 家服务商（阿里云/腾讯云/华为云/百度云/Cloudflare/DuckDNS/Oray/Namecheap/NameSilo），解析记录 + IP 来源（公网查询/自定义 URL/网卡/固定 IP/DNS 解析） |
| 网盘同步 | 远程挂载（飞牛文件管理挂载网盘）→ 本地目录的单向增量同步：按 size+mtime 比对只复制变化文件、持久化索引跳过未变动项、实时变化同步（检测间隔 1-3600s 可设）或定时同步、可选镜像删除（首轮不删 + 删除数异常自动中止的安全阀）、空文件夹同步、排除规则、按字节实时进度与网速显示、动态优先级（面板忙自动让路） |
| 网络测速 | 下载 / 上传 / 延迟 / 抖动 / 丢包五项指标；多测速节点并发 TCP 握手竞速选优（含四川成都联通），NAS 后端直连节点跑流量（8 连接并发、取稳态速率），浏览器只回传结果不占网关中转带宽；历史记录本地保存（最近 30 条），支持中止与清空 |
| 浏览器 | 内网网页代理浏览（`/b/{scheme}/{host}/…` 路径式同源代理 + `<base>` 注入 + 全量 URL 重写 + JS 运行时接管）：远程域名下也能打开路由器管理页等内网站点，支持多标签页、Ctrl+滚轮缩放、链接一律在面板内新标签打开；Cookie 按站点隔离，登录表单与 WebSocket 长连接可用 |
| 终端 | 命令执行控制台（root 身份，`sh -c`，cd 会话目录保持，单命令 60s 超时，长任务建议 `nohup … &` 后台）；显式「连接/断开」控制 |
| 掉盘修复 | NVMe 盘状态枚举与掉线标注；**按盘自选**禁止/恢复 D3cold 深睡（`d3cold_allowed=0`，只禁深睡、保留浅层省电），udev 规则持久化，重启后依旧生效 |
| 系统设置 | 检查 GitHub Release 新版本（版本对比 + 更新说明），后台异步下载新版本 fpk（带进度轮询）到 NAS 本地并引导覆盖安装 |

## 安全设计（清理硬约束）

1. **白名单根目录**：仅允许 `/tmp`、`/var/tmp`、`/var/log`、`/var/cache/apt`、`/var/crash`、`/var/lib/systemd/coredump` 及 `/vol*/@appdata/<app>/{logs,tmp}`、`/vol*/@appmeta/*/data/{thumb,face}` 等；
2. **只删普通文件**：Lstat（不跟随符号链接），跳过目录/socket/设备，绝不递归删目录树；
3. **年龄阈值双重复核**：每个分类有最小文件年龄，扫描与清理两阶段各自复核；
4. **扫描时效**：清理只接受 30 分钟内的扫描结果，过期须重新扫描；
5. journal 类固定保留最近 500MB（按 mtime 降序累计）。

## 架构

```
src/
  main.go            路由注册/启动/版本常量（manifest 版本自动同步源）/隧道与表单桥接中间件
  overview.go        仪表盘数据（CPU/内存/磁盘/端口/网络）
  net.go             网卡/网关/DNS/速率（ip route 优先，/proc 回退）
  temps.go           CPU/硬盘温度（hwmon realpath 对齐 + smartctl + drivetemp）
  nat.go             STUN NAT 检测（纯标准库 RFC3489/5389）+ 公网 IP 查询
  junk.go            垃圾清理规则引擎（白名单/年龄/明细截断）
  procs.go           进程/服务（250ms 双采样实时 CPU%）
  ports.go           端口审计
  autostart.go       开机启动项
  ddns_core.go       DDNS 记录引擎
  ddns_providers.go  9 家服务商 API 实现
  browser.go         内网网页代理（路径式同源代理 + 全量 URL 重写 + JS 运行时接管）
  terminal.go        终端命令执行控制台（cd 会话/互斥/超时）
  sync.go            网盘同步（增量比对/实时与定时调度/镜像删除安全阀）
  speedtest.go       网络测速（节点竞速/多连接带宽/延迟抖动丢包/历史）
  updater.go         版本检查与后台异步下载（进度轮询）
  nvme_sleep.go      掉盘修复（d3cold_allowed + udev 持久化）
  web/index.html     内嵌 Web UI（单文件，零构建）
pkg/                 fpk 打包目录（manifest / cmd 脚本 / 图标资源 / wizard 向导）
build.sh             交叉编译 + fnpack 打包（自动同步 manifest 版本、生成图标）
```

卸载时应用中心会弹出「是否清除应用数据」选项（`pkg/wizard/uninstall` 向导）：
默认保留整理任务、DDNS 配置、网盘同步配置、测速历史与日志（重装可恢复），
选择删除时才由 `pkg/cmd/uninstall_callback` 清空数据目录。卸载不会删除
任何被整理过的媒体文件本身。

源码目录只放产品源码与打包定义；开发侧的测试（`../tests/`）与工具脚本
（`../tools/`：自检、图标生成、发布、歌手数据抓取）按约定放在仓库外，
安装包产物输出到 `../dist/`。

## 构建

```bash
bash build.sh      # 交叉编译 amd64/arm64 → fnpack 打包 → ../dist/X-Box.fpk
bash ../tools/selfcheck.sh    # 46 项自检（结构/JSON/二进制/图标一致性/fpk/安全约束）
python3 ../tests/tunnel_test.py       # 真实 linux 二进制：API 隧道/终端/浏览器代理/异步下载（建议 WSL）
node ../tests/e2e_panel.test.js       # 端到端：无登录直开面板/三通道回退/隧道编解码/语法地板
node ../tests/smoke_test.js           # 前端渲染冒烟测试（边界数据）
```

依赖：Go 1.22+（`CGO_ENABLED=0` 交叉编译）、fnpack 1.2.3。

## 安装

飞牛应用中心 → 手动安装 → 选择 `X-Box.fpk`（run-as root）。安装后从桌面图标进入：入口经 fnOS 统一网关（unix socket + 飞牛账号登录态），应用自身不监听任何 TCP 端口。

**接入方式（fnOS 统一网关）**：入口声明在 `app/ui/config` 的 `gatewaySocket` + `gatewayPrefix`，应用监听 `${TRIM_APPDEST}/app.sock`（Unix Socket，权限 0770），**不对外占用端口**。请求先由 fnOS 网关校验飞牛账号登录态，再以 `/app/{appname}/...` 前缀转发进 socket；应用剥掉前缀后正常路由，并把前缀补回所有对外生成的绝对路径（代理路径、Cookie Path、`<base>`、注入脚本）。

这带来三个结果：

- 外部入口（官方域名、自有域名、IPv6、PC/手机客户端）全部由网关统一承接，**只要飞牛账号已登录即可访问**；
- 应用不监听公网地址，攻击面收敛到网关；
- 应用自身没有登录态，身份由网关注入的 `X-Trim-Userid` / `X-Trim-Username` 头表达（业务侧不使用）。

**不监听任何 TCP 端口**（8899 与直通端口均已下线）：唯一入口是网关 socket。自动化测试通过 `XBOX_DEV=1` 临时开启回环端口，生产形态不启用。

**关于 fnOS 客户端的 API 隧道**：fnOS 远程访问中转会拦截 `/api/` 前缀（重定向到跨域门户，导致页面内 XHR/fetch 请求挂起），而 `/` 路径畅通。本应用的全部 API 请求因此编码进 `/?__x=<base64url(JSON)>` 隧道参数，由服务端 `tunnelBridge` 中间件还原分发；无效隧道自动回落页面。直连 `http://<NAS IP>:8899` 的浏览器访问同样支持（隧道对直连透明）。

## 已知兼容性说明

- 浏览器代理为轻量级实现：绝大多数站点（含路由器管理页、GBK 老页面、需登录的表单、WebSocket 长连接）可用。按路径注册路由的单页应用（自带桌面的管理界面等）在网关前缀下无法保持根路径，此类站点请在 fnOS 对应的应用入口打开。重度依赖 WebGL/Service Worker 的页面仍可能显示不全。
- 终端为命令执行控制台（无 PTY）：交互式全屏程序（vim/htop 等）不可用，长任务用 `nohup … &` 后台运行
- 部分国产公方案 NVMe（PCI ID `1d89:ef25` 等）存在 D3cold 唤不醒缺陷，空闲约 1 小时后掉盘；「掉盘修复」页可对指定盘禁用 D3cold 规避
- 硬盘温度依赖 hwmon/smartctl/drivetemp，传感器或工具缺失时显示「—」并可在悬浮提示查看原因

## License

MIT，详见 [LICENSE](LICENSE)。
