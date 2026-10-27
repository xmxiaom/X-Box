# X-Box — 飞牛 fnOS 系统管理工具箱

单 Go 二进制 + 内嵌 Web UI（**纯 Go 标准库，零第三方依赖**），以 fnOS 原生应用（fpk）形式安装运行，内置 amd64 / arm64 双架构。系统清理、安全体检、DDNS、网盘同步、媒体整理、内置 BT 下载、内网浏览器与 root 终端，一个面板全搞定。

## 功能

| 页面 | 功能 |
|---|---|
| 仪表盘 | CPU（含温度 3s 动态刷新）/内存/负载/运行时长、磁盘用量、监听端口与公网暴露概况、网络信息（网卡/IPv4/IPv6/MAC/网关/DNS/**上下行速率每秒刷新**）、NAT 类型检测（STUN，NAT1–NAT4 等级 + CGNAT 判定 + 优化建议）、存储空间与硬盘温度 |
| 垃圾清理 | 9 类安全分类扫描（apt 缓存、轮转日志、journal、tmp、crash、应用日志/tmp/缩略图、已卸载应用残留），文件级勾选清理 + 全清，白名单 + 年龄阈值双重复核；卸载残留按整目录删除，删前重新判定应用是否已重新安装 |
| 端口管理 | 解析 /proc/net 全部 tcp/tcp6/udp/udp6，进程映射、属主、暴露面（公网/局域网/仅本机）分类 |
| 进程与服务 | 全进程 CPU 3s 实时刷新（搜索/排序）、状态按「运行中/等待」区分并标出所属飞牛应用、系统线程可隐藏，systemd 服务、飞牛应用（已安装/卸载残留分开，残留可一键跳转清理）、Docker 容器一览，支持结束进程 |
| 开机自启 | systemd 服务/定时器、rc.local、cron @reboot、init.d、fnOS 应用启动项扫描 |
| DDNS 服务 | 9 家服务商（阿里云/腾讯云/华为云/百度云/Cloudflare/DuckDNS/Oray/Namecheap/NameSilo），解析记录 + IP 来源（公网查询/自定义 URL/网卡/固定 IP/DNS 解析） |
| 网盘同步 | 远程挂载（飞牛文件管理挂载网盘）→ 本地目录的单向增量同步：按 size+mtime 比对只复制变化文件、持久化索引跳过未变动项、实时变化同步（检测间隔 1-3600s 可设）或定时同步、可选镜像删除（首轮不删 + 删除数异常自动中止的安全阀）、空文件夹同步、排除规则、按字节实时进度与网速显示、动态优先级（面板忙自动让路） |
| 视频下载 | 磁力聚合搜索（TPB / YTS / BitSearch / Nyaa / 动漫花园 五源并行）+ **内置 BT 引擎直下**（无需安装 qBittorrent/Transmission/aria2）；DHT + BEP15 UDP tracker 找 peer，BEP9 抓种子信息并落盘缓存（同资源重试秒进下载态）；**下载列表支持单任务暂停/继续/停止/移除与批量全部暂停/继续/停止/清除完成/清空**，暂停保留已完成分片、继续从断点续传；同时下载数、单任务并发连接数、单连接挂起块数均可在设置里调 |
| 音乐下载 | 三源并行搜索（网易云曲库 + 酷我免费直链库 + 磁力库 TPB音频/Nyaa）；免费曲直下 mp3（HTTP Range 断点续传，支持暂停/继续），VIP 曲优先走酷我同曲直链、再兜底磁力；任务列表与视频下载共用引擎与同一套控制按钮 |
| 视频管理 | 读文件名/目录名与影片内嵌信息（MP4/MKV），按 Jellyfin/Plex 规范重命名：电影「片名 (年份) [分辨率] [来源]」、剧集「剧名 (年份) - S01E01 - 标题」+ `Season 01`；纯数字文件名在目录有剧名证据时当集数、尾部裸集数只认前导零或三位数（避免「速度与激情 10」误判）；花絮归入 `extras/`、sample 跳过；默认先预览再执行，只改名/移动，绝不删除 |
| 音乐管理 | 读音频标签（MP3/FLAC/M4A/WMA）与文件名交叉印证，内置 1450+ 条中英文歌手别名表；纠正「歌名与歌手填成同一个值」「标题框是原始文件名」等可判定错位；按「歌名 - 歌手」重命名（模板可改），可选归档到「歌手/专辑」或平铺回一层；GBK 乱码自动识别修复；只改文件名与标签，绝不删除 |
| 网络测速 | 下载 / 上传 / 延迟 / 抖动 / 丢包五项指标；多测速节点并发 TCP 握手竞速选优（含四川成都联通），NAS 后端直连节点跑流量（8 连接并发、取稳态速率），浏览器只回传结果不占网关中转带宽；历史记录本地保存（最近 30 条），支持中止与清空 |
| 浏览器 | 内网网页代理浏览（`/b/{scheme}/{host}/…` 路径式同源代理 + `<base>` 注入 + 全量 URL 重写 + JS 运行时接管）：远程域名下也能打开路由器管理页等内网站点，支持多标签页、Ctrl+滚轮缩放、链接一律在面板内新标签打开；Cookie 按站点隔离，登录表单与 WebSocket 长连接可用 |
| 终端 | **PTY 交互终端**（长驻 `script -qec bash` 伪终端，vim/htop 等全屏程序可用）+ 命令执行控制台双模式；cd 会话目录保持、单命令超时保护、5 分钟空闲回收；`script` 缺失时自动降级为 exec 模式 |
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
  net.go             网卡/网关/DNS/速率（ip route 优先，/proc 回退；速率按真实采样间隔算）
  temps.go           CPU/硬盘温度（hwmon realpath 对齐 + smartctl + drivetemp）
  nat.go             STUN NAT 检测（纯标准库 RFC3489/5389）+ 公网 IP 查询
  junk.go            垃圾清理规则引擎（白名单/年龄/明细截断/整目录清理）
  procs.go           进程/服务（双采样按真实间隔算 CPU%、应用归属、系统线程识别、应用安装状态判定）
  ports.go           端口审计
  autostart.go       开机启动项
  ddns_core.go       DDNS 记录引擎
  ddns_providers.go  9 家服务商 API 实现
  browser.go         内网网页代理（路径式同源代理 + 全量 URL 重写 + JS 运行时接管）
  browserws.go       代理链路的 WebSocket 长连接桥接
  terminal.go        终端（PTY 长驻 script 会话 + exec 降级双模式）
  cloudsync.go       网盘同步（增量比对/实时与定时调度/镜像删除安全阀）
  speedtest.go       网络测速（节点竞速/多连接带宽/延迟抖动丢包/历史）
  updater.go         版本检查与后台异步下载（进度轮询）
  nvme_sleep.go      掉盘修复（d3cold_allowed + udev 持久化）
  media.go           媒体整理调度（任务=目录+开关，先预览后执行）
  media_video.go     视频整理（片名/年份/季集解析、Jellyfin/Plex 命名、extras 归类）
  media_audio.go     音频整理（标签与文件名交叉印证、歌手判定、模板重命名）
  media_id3.go       ID3v2 标签读写（synchsafe 长度、UTF-16 BOM 字节序）
  media_vorbis.go    FLAC/Vorbis 注释块重写（按原块长度推进、重打 last-block）
  media_asf.go       WMA/ASF 标签（Header 长度与 File Properties 大小同步）
  media_mp4.go       MP4/M4A atom 解析与标签读写
  media_matroska.go  MKV 内嵌信息读取
  media_tags.go      标签统一抽象层
  media_correct.go   标签纠错判定（可判定才改，无法判定只提示）
  media_meta_api.go  MusicBrainz/iTunes 联网匹配
  charset.go         字符集嗅探与转换
  charset_gbk.go     GBK 被误当单字节解码后的还原判定
  artist_dict.go     内置歌手/组合名表
  artist_alias_data.go 生成的歌手别名表（1450+ 条）
  dlsearch.go        磁力聚合搜索（TPB/YTS/BitSearch/Nyaa/动漫花园 五源并行）
  dlapi.go           下载 API（搜索/配置/任务列表/暂停/继续/停止/移除/批量控制）
  dlclient.go        下载配置（目录 + 并发/线程参数）
  dlmusic.go         音乐搜索与直链下载（网易云/酷我/磁力兜底，Range 续传）
  btcore.go          BT 基础（bencode 编解码、磁力解析、metainfo、文件名清洗）
  btengine.go        内置 BT 引擎（任务状态机、tracker 双轨、worker 池、暂停续传、并发调优）
  btwire.go          BT 线协议（BEP3 握手/BEP10 扩展/BEP9 ut_metadata）
  btdht.go           DHT（KRPC 最小 Kademlia、节点池保活）
  btmetacache.go     种子信息磁盘缓存（SHA1 复核，损坏自愈）
  btmetadiag.go      元数据抓取诊断工具（开发用，不设环境变量不触发）
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
安装包产物 `X-Box.fpk` 直接生成在源码目录根下（便于取用，已被 `.gitignore`
排除不会上传 GitHub）。

## 构建

```bash
bash build.sh      # 交叉编译 amd64/arm64 → fnpack 打包 → X-Box.fpk（源码目录根下）
bash ../tools/selfcheck.sh            # 50 项自检（结构/JSON/二进制/图标一致性/fpk/安全约束）
python3 ../tests/tunnel_test.py       # 真实 linux 二进制：API 隧道/终端/浏览器代理/异步下载（建议 WSL）
node ../tests/e2e_panel.test.js       # 端到端：无登录直开面板/三通道回退/隧道编解码/语法地板
node ../tests/smoke_test.js           # 前端渲染冒烟测试（边界数据）
node ../tests/media_page.test.js      # 媒体管理页：子标签/预览/纠错/平铺/解析
node ../tests/dl_music_page.test.js   # 音乐下载页：搜索渲染/酷我分区/下载载荷
node ../tests/dl_task_page.test.js    # 下载列表：弹窗/暂停续传按钮/批量控制/并发设置
```

依赖：Go 1.22+（`CGO_ENABLED=0` 交叉编译）、fnpack 1.2.3、Node.js（仅前端测试用 jsdom）。

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
- 终端默认走 PTY 模式（长驻 `script -qec bash`），vim/htop 等全屏交互程序可用；若系统缺少 util-linux 的 `script` 命令会自动降级为命令执行模式（`sh -c`，单命令超时保护，长任务用 `nohup … &`）
- 内置 BT 引擎为纯标准库实现，支持磁力/BEP9 元数据/BEP15 UDP tracker/DHT；种子总大小上限 4GiB，同时任务数上限 8。冷门资源仍取决于是否有做种在线，与任何 BT 客户端一致
- 音乐下载受版权限制：网易云 VIP/付费曲的官方外链会 302 到 /404，此时会提示并引导走「酷我免费直链」或「磁力」两条替代路径，不会写入残缺文件
- 部分国产公方案 NVMe（PCI ID `1d89:ef25` 等）存在 D3cold 唤不醒缺陷，空闲约 1 小时后掉盘；「掉盘修复」页可对指定盘禁用 D3cold 规避
- 硬盘温度依赖 hwmon/smartctl/drivetemp，传感器或工具缺失时显示「—」并可在悬浮提示查看原因

## License

MIT，详见 [LICENSE](LICENSE)。
