# X-Box — 飞牛 fnOS 非 Docker 系统清理与安全体检工具箱

单 Go 二进制 + 内嵌 Web UI（**纯 Go 标准库，零第三方依赖**），以 fnOS 原生应用（fpk）形式安装运行，内置 amd64 / arm64 双架构。

## 功能

| 页面 | 功能 |
|---|---|
| 仪表盘 | CPU（含温度 3s 动态刷新）/内存/负载/运行时长、磁盘用量、监听端口与公网暴露概况、网络信息（网卡/IPv4/IPv6/MAC/网关/DNS/实时速率）、NAT 类型检测（STUN，NAT1–NAT4 等级 + CGNAT 判定 + 优化建议）、存储空间与硬盘温度 |
| 垃圾清理 | 8 类安全分类扫描（apt 缓存、轮转日志、journal、tmp、crash、应用日志/tmp/缩略图），文件级勾选清理 + 全清，白名单 + 年龄阈值双重复核 |
| 端口管理 | 解析 /proc/net 全部 tcp/tcp6/udp/udp6，进程映射、属主、暴露面（公网/局域网/仅本机）分类 |
| 进程与服务 | 全进程 CPU 3s 实时刷新（搜索/排序）、systemd 服务、飞牛应用、Docker 容器一览，支持结束进程 |
| 开机自启 | systemd 服务/定时器、rc.local、cron @reboot、init.d、fnOS 应用启动项扫描 |
| DDNS 服务 | 9 家服务商（阿里云/腾讯云/华为云/百度云/Cloudflare/DuckDNS/Oray/Namecheap/NameSilo），解析记录 + IP 来源（公网查询/自定义 URL/网卡/固定 IP/DNS 解析） |
| 反向代理 | 多域名站点转发（通配符/路径前缀/去前缀），可联动 DDNS 自动建记录 |
| 掉盘修复 | NVMe 盘状态枚举与掉线标注；**按盘自选**禁止/恢复 D3cold 深睡（`d3cold_allowed=0`，只禁深睡、保留浅层省电），udev 规则持久化，重启后依旧生效 |

## 安全设计（清理硬约束）

1. **白名单根目录**：仅允许 `/tmp`、`/var/tmp`、`/var/log`、`/var/cache/apt`、`/var/crash`、`/var/lib/systemd/coredump` 及 `/vol*/@appdata/<app>/{logs,tmp}`、`/vol*/@appmeta/*/data/{thumb,face}` 等；
2. **只删普通文件**：Lstat（不跟随符号链接），跳过目录/socket/设备，绝不递归删目录树；
3. **年龄阈值双重复核**：每个分类有最小文件年龄，扫描与清理两阶段各自复核；
4. **扫描时效**：清理只接受 30 分钟内的扫描结果，过期须重新扫描；
5. journal 类固定保留最近 500MB（按 mtime 降序累计）。

## 架构

```
src/
  main.go            路由注册/启动/版本常量（manifest 版本自动同步源）
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
  proxy.go           反向代理（nginx/caddy 风格 hosts 匹配）
  nvme_sleep.go      掉盘修复（d3cold_allowed + udev 持久化）
  web/index.html     内嵌 Web UI（单文件，零构建）
pkg/                 fpk 打包目录（manifest / cmd 脚本 / 图标资源）
tests/               冒烟测试（Node DOM 桩）+ mock server（视觉验证）
build.sh             交叉编译 + fnpack 打包（自动同步 manifest 版本）
selfcheck.sh         46 项自检
```

## 构建

```bash
bash build.sh      # 交叉编译 amd64/arm64 → fnpack 打包 → X-Box.fpk
bash selfcheck.sh  # 46 项自检（结构/JSON/二进制/图标一致性/fpk/安全约束）
node tests/smoke_test.js   # 前端冒烟测试（Node 22+，无浏览器依赖）
```

依赖：Go 1.22+（`CGO_ENABLED=0` 交叉编译）、fnpack 1.2.3。

## 安装

飞牛应用中心 → 手动安装 → 选择 `X-Box.fpk`（run-as root）。安装后从桌面图标进入，服务端口 8899。

**安全提示**：应用无访问令牌登录，面板监听 `0.0.0.0:8899`，局域网内任何主机均可访问——**请勿将 8899 端口暴露到公网**。

## 已知兼容性说明

- 部分国产公方案 NVMe（PCI ID `1d89:ef25` 等）存在 D3cold 唤不醒缺陷，空闲约 1 小时后掉盘；「掉盘修复」页可对指定盘禁用 D3cold 规避
- 硬盘温度依赖 hwmon/smartctl/drivetemp，传感器或工具缺失时显示「—」并可在悬浮提示查看原因

## License

MIT，详见 [LICENSE](LICENSE)。
