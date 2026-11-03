# 从 EVE-RedisQ 到 officerwatch

一份改造记录，留着解释"为什么这版长这样"。前身代码见仓库外 `EVE-RedisQ.tar`
（2025-11 起、2026-02 停更，Windows 单 exe，4 个文件 2943 行）。

## 一、结构对照

| 前身 | 现在 |
|---|---|
| 单包 `main`，4 个文件 | 8 个包 + `cmd`，按"数据源 / 存储 / 匹配 / 通知 / HTTP"分层 |
| 1700 行 `gui_http.go`，HTML 写在字符串常量里 | `internal/web`（API）+ `internal/web/assets`（静态前端，`go:embed`） |
| `main_gui.go` 是唯一入口，README 说的 `--no-gui` 根本不存在 | 有 `-config/-listen/-version/-import-legacy/-sync-catalog`，README 与代码一致 |
| 全局 `log.SetOutput` + `io.MultiWriter` 把日志推到 WebSocket | `slog` → journald；页面有独立的事件表（结构化，可筛选） |
| GUI 是浏览器里的一个页面，绑 `0.0.0.0:8080`，无鉴权 | 绑 `127.0.0.1:8795`，由 Nginx 子路径 + 站点 Basic Auth 保护 |
| 无测试 | 408 行单测：匹配、口径、幂等、名录开关、文案、限额 |

## 二、坏味道 → 处理

| 前身的问题 | 现在 |
|---|---|
| 手写 gzip/deflate 解压，还复制了三份（因为手动设了 `Accept-Encoding`，把 Go 的自动解压关了） | 只保留 Go 默认的自动解压；需要伪装头的地方不碰 `Accept-Encoding` |
| `shouldNotify` 在 Debug 模式直接 `return true` → 忘关一次就是全量轰炸 | 没有"全量旁路"，调试靠单测和事件表；口径只能通过页面上的显式规则改 |
| 空响应重试 50–200ms，违反 RedisQ 2 req/s 的限制 | R2Z2 序列流：404 睡 6 秒、每条间隔 100ms（上限 10/s，官方限 15/s） |
| `gm.messageRecords` 从采集 goroutine 里 append、HTTP handler 并发读，无锁 | 数据只在 SQLite 里，HTTP 只读库；进程内不留并发共享切片 |
| `StopService` 往无缓冲 channel 发送，监听循环已退出时永久阻塞 | 一个 `signal.NotifyContext` 控制全部协程，退出顺序固定 |
| `INSERT OR REPLACE` 每次写都换掉自增 id | 主键就是 `killmail_id`，冲突即忽略（幂等），无自增主键 |
| 过滤逻辑在订阅/导入/重发三处各写一遍 | 一套 `collect.process`，导入与采集共用；口径判断抽成纯函数 `Decide` 并有单测 |
| 无效过滤规则默认放行、`fmt.Sscanf` 忽略错误 | 配置项有校验（role/时段/阈值范围），非法直接 400 |
| `solar_system_id` 解析了从不使用 → 告警里没有"在哪" | 星系 + 星域都解析并缓存（`name_cache`），邮件/表格/详情都带地点 |
| 群 token 硬编码在 HTML 的 placeholder 里，编译进 exe | 凭据只在 `config.json`（0600），API 只返回"已配置/未配置"，页面永不接触明文 |
| 报告 403/429 时只打日志，看不清到底怎么了 | 采集计数 + 最近错误 + 事件表，状态页一眼看到 |
| 文档漂移（README 说 Fyne v2、说支持 `--no-gui`） | README 描述的入口、端口、路径都与线上一致 |

## 三、保留的产品判断

- **只做"读公开信息 → 本地算 → 提醒人"**，不碰游戏进程、不做自动操作；
- **按官员船型认人**，而不是维护角色 ID 表 —— 这条判断被 ESI 的 `Officer` 船组做得更彻底；
- **被过滤的也要留痕**（前身记为"已过滤"，这版记 `filtered` + 原因）；
- **手工 KillID 导入 + 重发**：作为兜底手段保留，并顺手用来补全老数据。

## 四、丢弃的东西

| 丢掉 | 原因 |
|---|---|
| RedisQ 长轮询 | 那台机器上 TLS 被重置；且天然抽样会漏 |
| OneBot / QQ 群 | 依赖朋友的机器人服务端，条件已不具备。代码保留，填配置即可启用 |
| Fyne GUI | 从没真正用过（README 写了，代码里没有）；浏览器页面更省事 |
| `kb` / `kb-subscriber` 两个 exe | 同一份代码的两种构建，现在一个二进制搞定 |

## 五、还没做的

- 官员图鉴里的"掉落物预期价值"没有内置数据 —— 需要物品价格源，属于另一个项目（`eve-money-money`）的领域；
- `officer_down`（玩家打死官员）默认关闭：这类击杀少但价值高，先把开关交给人；
- 历史数据只有 15 条（前身采样率决定的），不会去 zkillboard 做全量回填。
