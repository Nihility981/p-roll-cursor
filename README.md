# cursor-switch (Go PoC)

用 Go 重写 Cursor 的账号切换 / 用量查询工具。项目的核心需求是**账号切换**，
**已在 2026-08-17 实现并验证**（`cmd/acct switch`）。在此之前完成的只读探测、
用量口径解析等，都是为它铺路的地基。

后续规划：OAuth 登录 → Wails v3 GUI。

## 安全边界

这个项目有两类命令，边界不一样，**不要混为一谈**：

| 命令 | 对编辑器数据文件 | 说明 |
| --- | --- | --- |
| `cmd/probe` | **纯只读** | 只读 `state.vscdb`，除 `-out` 留档目录外不写任何文件 |
| `acct save` / `list` / `import` | **只读** | `save` 以只读方式打开 `state.vscdb`；只写我们自己的账号库 |
| `acct switch` / `at` / `rollback` | **写入 `state.vscdb`** | 这是它们的本职工作，见下 |

早期版本的 README 写的是「全程只读」，那是探测阶段的约束。**切号功能落地后，
`switch` 与 `rollback` 必然要写 `state.vscdb`**，否则功能无从谈起。现在的约束是：

- 写入范围**严格限定**在 8 个 `cursorAuth/*` key（6 写 2 删），不碰库里其他任何数据；
- **不复制整个数据库**（本机 3.5 GB，复制一次好几秒还白占空间）。备份只存那几个
  key 的旧值，一个几百字节的带时间戳 JSON，可用 `acct rollback` 完整还原；
- 写入前**必须确认编辑器已退出**，默认拒绝执行而不是替用户关掉编辑器；
- 除非用户显式加 `-kill` / `-start`，否则**不启动、不终止编辑器进程**；
- 终端输出与落盘文件中的 token、邮箱、用户 ID 一律**明文**（这是纯本地的个人工具，
  查的是本机自己的账号，遮蔽只会妨碍排查）。因此 `probe-output/` 会包含明文凭据，
  已由 `.gitignore` 排除，**切勿提交、切勿外发**。

账号库（默认 `%APPDATA%\cursor-switch\accounts.json`）是本工具自己的产物，
位置在编辑器数据目录和本仓库之外，写它与上面的边界无关。

## 安装

模块路径是 `github.com/Nihility981/p-roll-cursor`，与仓库地址一致，所以可以直接
`go install` 装成命令行工具：

```powershell
go install github.com/Nihility981/p-roll-cursor/cmd/acct@latest    # 账号切换
go install github.com/Nihility981/p-roll-cursor/cmd/probe@latest   # 只读探测
```

装完二进制在 `$(go env GOPATH)\bin` 下（默认 `%USERPROFILE%\go\bin`），
把这个目录加进 `PATH` 就能直接敲 `acct` / `probe`。
最快切号：`acct at`（提示粘贴 accessToken）或 `acct at <accessToken>`，导入 → 切号 → 重启编辑器。

### 前提：这是私有仓库，必须先配 GOPRIVATE

**这一步不能省。** 默认情况下 `go install` 会走公共代理 `proxy.golang.org` 和校验
数据库 `sum.golang.org`，而它们**看不到私有仓库**，结果是 404 或校验失败：

```powershell
go env -w GOPRIVATE=github.com/Nihility981/*
```

`GOPRIVATE` 会同时把该前缀从 `GOPROXY` 和 `GOSUMDB` 里排除掉，让 go 直接用 git 拉源码。

因此**本机 git 还必须能认证到 GitHub**，否则 go 拉取时会卡在鉴权上。二者选一：

```powershell
# 方式一：让 git 用 SSH 代替 HTTPS（已配好 SSH key 时最省事）
git config --global url."git@github.com:".insteadOf "https://github.com/"

# 方式二：用 PAT 走 HTTPS，凭据交给 Windows 凭据管理器
git config --global credential.helper manager
# 首次 git clone 私有仓库时输入 PAT 作为密码，之后 go 就能复用
```

验证是否配通：`git ls-remote https://github.com/Nihility981/p-roll-cursor` 能列出
引用即说明认证没问题，此时 `go install` 才会成功。

`@latest` 取的是最新的 **git tag**；仓库还没打 tag 的话用 `@main` 指定分支。

## 本地开发运行

在仓库目录里直接跑，不需要 `go install`：

```powershell
cd go-cursor
$env:CGO_ENABLED = "0"
go run ./cmd/probe
go run ./cmd/acct list
```

可选参数：

| 参数 | 说明 |
| --- | --- |
| `-out <dir>` | 原始响应留档目录，默认 `probe-output` |
| `-offline` | 只做本地 `state.vscdb` 读取，跳过全部网络请求 |

输出分 6 段：环境信息 → 本机登录态 → JWT 解析 → API 调用 → 结构化用量 → 原始响应留档。
最后会把 usage-summary 的原始 JSON（明文）写到 `probe-output/usage-raw.json`。

多次运行的输出顺序是稳定的（所有 key 列表都做了排序），可以直接 `diff` 比对两次结果。

### 依赖与构建

首次运行需要联网执行一次 `go mod tidy`，之后就走本地模块缓存：

```powershell
$env:CGO_ENABLED = "0"
go mod tidy
go test ./...
go build ./...
```

SQLite 驱动是 `github.com/ncruces/go-sqlite3 v0.23.3`（纯 Go + wazero，**不需要 CGO**）。
它和传递依赖（`github.com/tetratelabs/wazero`、`github.com/ncruces/julianday`）都在 github.com 上，
公司网只放行 GitHub、下不了 `modernc.org/sqlite` 时用这个。
另外还有本项目自己用的 `golang.org/x/sys`（Windows 注册表），走 GOPROXY。
钉在 v0.23.3 是因为更新的 ncruces 把 `go` 指令抬到了 1.23+（最新要 1.25），本仓库保持 1.22。

### 已知环境陷阱：GOPATH 落在故障磁盘上

如果 `GOPATH` / `GOMODCACHE` 指向一块有问题的磁盘，`go mod tidy` 会**长时间挂起**
（实测干等 6 分钟没有任何输出），最终报出这样的错：

```
go: github.com/Nihility981/p-roll-cursor/internal/vscdb imports
        github.com/ncruces/go-sqlite3/driver: open <GOMODCACHE>\...\go-sqlite3\@v\v0.23.3.lock: The device is not ready.
```

**这个报错完全不像磁盘问题，极易误判成网络故障或代理配置错误**，从而白白浪费时间去
折腾 `GOPROXY`。判断方法：在同一个缓存目录里新建一个文件试试——如果新建成功、
只有那个 `.lock` 文件打不开，那就是磁盘/文件系统的问题，换代理救不了。

解决办法是把模块缓存挪到健康的磁盘上：

```powershell
$env:GOPATH = "C:\Users\<you>\go"
$env:GOMODCACHE = "C:\Users\<you>\go\pkg\mod"
go env GOPATH GOMODCACHE GOCACHE   # 确认三个都不在故障盘上
```

同一个症状还会连累看起来毫不相干的命令（实测连 `Get-Process` 都要 110 秒），
因为挂住的 `go.exe` 会在内核层堆积 I/O；先把它杀掉再排查。

## 目录结构

```
go-cursor/
  cmd/probe/            只读探测 CLI 入口
  cmd/acct/             账号切换 CLI：save / list / import / switch / at / rollback
  internal/paths/       Cursor 数据目录解析（Windows/macOS/Linux）
  internal/vscdb/       state.vscdb 访问：vscdb.go 只读，write.go 读写（切号用）
  internal/switcher/    切号编排：备份旧值 → 写库 → 删缓存 → checkpoint（含单元测试）
  internal/jwtutil/     JWT payload 解析（纯标准库）
  internal/cursorapi/   四个 Cursor HTTP 接口的 client
  internal/usage/       usage-summary 解析与套餐徽章归一化（含单元测试）
  internal/acctstore/   账号库：原子写 + 可替换编解码器（含单元测试）
  internal/procutil/    Cursor.exe 三级查找 + 进程识别 + 关闭/启动（含单元测试）
  internal/mask/        输出辅助：Shape 形态描述 + Outline JSON 结构概览
```

> `internal/mask` 这个包名是历史遗留——它当初负责脱敏，现在已经不做任何遮蔽了。
> 改名会牵动所有 import，留到后续单独处理。

## 账号切换（`acct` 命令）

### 推荐用法：一条命令切号

两种最快用法并列。

手上有 accessToken 就够了，不用先 `import`。最快用法是 `acct at` 或 `acct at <accessToken>`，内部等价于 `acct switch -token … -kill -start`。

```powershell
acct at
acct at "<accessToken>"
go run ./cmd/acct switch -token "<accessToken>" -email a@b.com -membership pro
```

它做三件事：从 token 构造账号 → 存进账号库（等价于跑一次 `import`）→ 执行完整的
写库切号。**执行前必须完全退出编辑器**，详见下面「真正切一次号的操作步骤」。

账号库里已有目标账号时，一条命令关进程 → 切号 → 再拉起：

```powershell
go run ./cmd/acct switch <邮箱或用户ID> -kill -start
```

`-email` / `-membership` / `-signup-type` 都可以省——省掉只是本机登录态少几个缓存
字段，切号本身不受影响（用户 ID 和 token 有效期都是从 JWT 里解出来的，不依赖它们）。
可以先加 `-dry-run` 看要做什么，那样连账号库都不会写。

同一用户 ID 再次直切会**直接覆盖**账号库里的旧记录并打印一行提示。理由是：用户显式
递了新 token 过来，意图就是用它登录，此时因为「已存在」而报错要求加 `-force` 只是白挡
一道，旧记录里存的必然是同一账号更早的 token。这与 `import` 默认拒绝的策略不同——
`import` 是纯归档动作，意图不明确，所以把决定权交回给人。

### 多账号管理：import + switch 两步

账号库里堆多个账号、来回切的场景仍然走两步，`switch` 的目标写邮箱或用户 ID：

```powershell
# 1. 把当前登录的账号存进账号库（对本机登录态只读）
go run ./cmd/acct save
go run ./cmd/acct save -force                     # 同一用户 ID 已存在时覆盖

# 2. 导入别的账号
go run ./cmd/acct import -token <accessToken> -email a@b.com -membership pro
go run ./cmd/acct import -file .\account.json     # {"items":{...}} 或 key/value 平铺

# 3. 看看有哪些账号可切
go run ./cmd/acct list

# 4. 切号——必须先完全退出编辑器
go run ./cmd/acct switch a@b.com -dry-run         # 先看要做什么，不写入
go run ./cmd/acct switch a@b.com                  # 真正写入
go run ./cmd/acct switch a@b.com -kill -start  # 关进程 → 切号 → 再拉起

# 5. 后悔了
go run ./cmd/acct rollback <备份文件>
```

邮箱撞车时 `switch` 会要求改用用户 ID，不会瞎猜。位置参数与 `-token` 是**二选一**，
同时给出会报错——一个是「从库里找已有账号」，一个是「用新 token」，程序不该替你猜。

### 真正切一次号的操作步骤

**`switch` 不能在编辑器内嵌的终端里跑**——那个终端本身就是编辑器的子进程，
关掉编辑器会连它一起带走。正确做法是先编译出独立二进制，再到编辑器外面执行：

```powershell
# 在编辑器里先编译好
go build -o $env:USERPROFILE\acct.exe ./cmd/acct

# 然后：完全退出编辑器（不是关窗口，是退出，任务栏托盘也要检查）
# 再打开一个独立的 PowerShell 窗口：
& $env:USERPROFILE\acct.exe at
# 或：& $env:USERPROFILE\acct.exe at "<accessToken>"
# 或者从账号库里已有的账号切（关进程 → 切号 → 再拉起）：
& $env:USERPROFILE\acct.exe list
& $env:USERPROFILE\acct.exe switch 目标邮箱 -kill -start
# 看到「切号完成」后也可手工重新启动
```

不想自己关编辑器的话可以用 `-kill -start` 让工具代劳，它会先发
WM_CLOSE 优雅关闭、超时才强杀。但**未保存的编辑仍可能丢失**，所以默认不这么做。

### 位置：刻意放在仓库外面

默认 `%APPDATA%\cursor-switch\accounts.json`，可用 `-store` 参数或环境变量
`CURSOR_SWITCH_STORE` 覆盖（优先级：参数 > 环境变量 > 默认）。目录不存在会自动创建。

放在仓库之外不是随手决定的：账号记录里存着 refresh token，**等同于账号密码**，
只要放进 `go-cursor/` 早晚会被 `git add`。放在用户目录下则物理上不可能被误提交。

### 格式：明文 JSON，但留好了加密的位置

当前是明文 JSON，与「取消全部脱敏」的既定策略一致，也便于排查。读写走 `Codec`
接口，将来要换成 AES-256-GCM 只需加一个实现——调用方和 `Account` / `File`
结构都不用动。文件头的 `encryption` 字段记录当前用的是哪种（现在恒为 `none`），
将来老文件才能被正确识别。

一条记录里存的是：

- `items`——`state.vscdb` 里认证 key 的**原始全量快照**（`save` 存 8 个，
  `import -token` 能补出 6 个）。切号时按下面「6 写 2 删」的清单取用，
  其余原样留着，信息不要丢；
- 派生字段——`userId`（去重主键）、`email`、`membershipType`、`signUpType`、
  `sub`、`tokenExpiresAt`、`exportedAt`。

**去重主键用 `userId` 而不是邮箱**：邮箱可以改，用户 ID 不会变。它由 JWT 的 `sub`
按 `|` 切最后一段得到。`tokenExpiresAt` 来自 JWT 的 `exp`，账号库里堆多个账号后
靠它一眼看出哪些 token 过期了，不用挨个去试。

### 重名策略：默认拒绝，`-force` 才覆盖

同一 `userId` 再次导出时默认**拒绝**，并把已有记录的导出时间与过期时间打出来。
理由是账号库目前是这些 refresh token 的唯一副本，而「同一个账号再导出一次」
既可能是想更新过期 token（该覆盖），也可能是误操作（不该覆盖）——程序分辨不出来，
所以把决定权交回给人。

### 原子写

先写同目录下的临时文件，`Sync` 后再 `rename` 覆盖，绝不就地改写。存凭据的文件
写到一半崩了会把整个账号库毁掉，而 refresh token 丢了就得把每个账号重新登录一遍。
临时文件必须和目标同目录，跨卷时 `rename` 不是原子操作。失败时清理临时文件，
不在用户目录里留垃圾。

### Windows 上的文件权限：并没有收紧，别误会

代码里写的 `0600` **在 Windows 上基本不起作用**——Go 不会把它翻译成 ACL，
新文件权限完全继承自父目录。本机实测 `%APPDATA%` 上显式挂着一条
`CodexSandboxUsers` 组的读权限 ACE 并向下继承，所以账号库的真实可访问者是
「当前用户 + SYSTEM + Administrators + 该组可读」，不是 `0600` 字面意义上的独占。

也就是说，这里**没有任何强制访问控制**。目前的实质保护只有两条：文件在用户
profile 内，以及它在仓库之外。真要收紧得显式设 ACL 或把内容加密。
（作为参照，Cursor 自己的 `state.vscdb` 在同一台机器上暴露面更大，同组是 `Modify`。）

## 关键实现约束

### state.vscdb 的打开方式

只读（`internal/vscdb/vscdb.go`，探测走这条）：

```
file:<path>?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)
```

读写（`internal/vscdb/write.go`，切号走这条）：

```
file:<path>?_pragma=busy_timeout(5000)
```

只是去掉 `mode=ro` 与 `query_only(1)`，其余约束一条不减。两条路径**分文件放置、
互不影响**：只读函数原样保留，谁在读、谁在写在代码结构上一眼可辨。

- 驱动是 `github.com/ncruces/go-sqlite3`（纯 Go + wazero，**不需要 CGO**），
  `database/sql` 驱动名是 `sqlite3`；DSN 的 `_pragma=` 写法与 ncruces 兼容；
- **绝不能加 `immutable=1`**：Cursor 显式开启了 WAL（同目录 `state.vscdb.options.json`
  内容为 `{"useWAL": true}`），`immutable` 会让 SQLite 忽略 `-wal` 文件，读到上一次
  checkpoint 之前的过期数据。实测佐证：连续几次运行之间 `cursorDiskKV` 的行数从
  290019 一路涨到 291264，说明读到的确实是实时数据；
- 必须 `db.SetMaxOpenConns(1)`；
- 路径要 `filepath.ToSlash()` 处理，否则 Windows 反斜杠会被 URI 解析吞掉。

### ItemTable 的 value 列：写入必须绑 `string`，绝不能绑 `[]byte`

建表语句是 `CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`，
即 **BLOB 亲和性**，尽管现存行的 `typeof(value)` 全是 `text`。

读的时候统一扫进 `[]byte` 再转 `string`，避免真 BLOB 出现时的隐式转换问题。
**写的时候则必须绑 `string`**：绑 `[]byte` 会被静默存成 blob，`typeof(value)`
变成 `'blob'`，Cursor 侧读出来可能是 Buffer 而不是字符串——不报错、不告警，
只是登录不上，属于最难排查的那类坑。Rust 的 rusqlite 传 `&str` 天然落成 TEXT，
所以参照实现从未暴露过这个问题，照抄它的逻辑不会提醒你注意这里。

`internal/switcher` 的测试里为此设了对照组：先用 `[]byte` 写一行、断言它确实
落成 `blob`（证明这个坑真实存在），再断言切号代码写出来的 6 个 key 全是 `text`。
切号代码本身在写完后也会复查一遍 `typeof`，不是 `text` 就报错并指向备份文件。

### 切号写哪些 key：6 写 2 删

实测存在的 8 个 `cursorAuth/*` key 全部有归属，不多不少：

| key | 处理 | 依据 |
| --- | --- | --- |
| `accessToken` | 写 | 凭据本体 |
| `refreshToken` | 写 | 实测与 accessToken 逐字节相同 |
| `cachedEmail` | 写 | 随账号变 |
| `cachedSignUpType` | 写 | 随账号变 |
| `stripeMembershipType` | 写 | 随账号变 |
| `stripeMembershipAuthId` | 写 | 与 JWT 的 `sub` 逐字节相同，属身份的一部分 |
| `cachedScopedProfile` | **删** | 账号绑定缓存，不删则新账号继承旧账号的 profile |
| `onboardingDate` | **删** | 同上 |

两点值得说明：

- **是 6 个而不是 5 个**：`stripeMembershipAuthId` 也要写。它虽然能被 Cursor
  自己重建，但我们手上正好有准确值，写进去才能让 8 个 key 全部自洽；
  只删不写会留下「有 token 却没有 authId」的中间状态。
- **删而不是写 profile**：`cachedScopedProfile`（形如 `{"displayName":"Example User"}`）
  我们没有新账号对应的正确内容，写旧值等于污染，交给 Cursor 登录后自建。

Rust 参照实现每次切号还会写 `stripeSubscriptionStatus`、`cursor.accessToken`、
`cursor.email` 三个 key——它们在新版库里根本不存在、Cursor 也从不读，纯属往库里加垃圾。

### 为什么切号前必须关 Cursor：**不是文件锁**

实测数据库**没有独占锁**，Cursor 运行时别的进程照样拿得到写句柄、照样写得进去。
真正的原因在应用层：**Cursor 把认证缓存在内存里，退出时会把旧值刷回数据库**，
把刚写入的新账号直接盖掉。所以顺序不能省：

```
确认 Cursor 已退出 → 备份旧值 → 写库(同一事务内写 6 删 2) → wal_checkpoint(TRUNCATE) → 启动 Cursor
```

默认行为是**检测到 Cursor 在跑就拒绝并提示**，而不是替用户把编辑器杀掉。

### 备份：只存那几个 key，不整库复制

库有 3.5 GB，整库备份既慢又白占空间。实际存的是一个带时间戳的小 JSON
（实测 819 字节），放在账号库同级的 `backups/` 下，记录 8 个 key 的旧值。
值为 `null` 表示该 key 切号前**不存在**——回滚时要删掉它而不是写空字符串，
「没有这个 key」和「值是空串」不是一回事。`acct rollback <文件>` 可完整还原。

### 认证 key 的真实名字

新版 Cursor（版本号**待确认**，此前记录为 3.15.19 但未经验证）里 **存在**：

`cursorAuth/accessToken`、`cursorAuth/refreshToken`（值与 accessToken 相同）、
`cursorAuth/cachedEmail`、`cursorAuth/stripeMembershipType`、`cursorAuth/cachedSignUpType`、
`cursorAuth/stripeMembershipAuthId`、`cursorAuth/cachedScopedProfile`、`cursorAuth/onboardingDate`

**不存在**（Rust 参照实现里写了但读不到，2026-08-16 实测逐一确认）：

`cursorAuth/stripeSubscriptionStatus`、`cursorAuth/authId`（真名是 `stripeMembershipAuthId`）、
`cursor.accessToken`、`cursor.email`

### usage-summary 的用量口径（最容易算错的地方）

真实响应里 `individualUsage.plan` 长这样：

```json
{"used": 2000, "limit": 2000, "remaining": 0,
 "apiPercentUsed": 33.535,
 "autoPercentUsed": 0.6928571428571428,
 "totalPercentUsed": 7.9911111111111115,
 "breakdown": {"included": 2000, "bonus": 5192, "total": 7192}}
```

#### 三个字段的真实含义

- **`breakdown` 是「已消耗用量」的构成，不是额度上限。** `included` 与 `bonus` 分别是
  从两个池子里**消耗掉**的量，`total = included + bonus`。这是最容易误读的一点。
- **`plan.limit` 只是 included 那一档的上限**，不是总额度。本机实测 included 的 $20
  已经用尽（`used=2000`、`limit=2000`、`remaining=0`），之后的 $51.92 全部走 bonus。
  所以 `used / limit` 恒等于 100%，**绝不能拿它当总体百分比的分母**。
- 拿 `breakdown.total` 当分母同样是错的（会算出 36.8%）——它是分子不是分母。

#### 分母结构：$900 = API $200 + auto $700

响应里**没有任何字段直接给出额度分母**，但它可以从响应自身反解出来。设 API 分母为
`Da`、auto 分母为 `Db`，三个百分比为 `a`/`b`/`t`，已消耗总量为 `T`：

```
T / (t/100) = Da + Db      总分母
a*Da + b*Db = T            两路用量相加等于总用量
```

两个方程两个未知数，直接解得 `Da = 20000`（$200）、`Db = 70000`（$700）、
总额 `90000`（$900）。响应里的两句文案印证了这个拆分：
`namedModelSelectedDisplayMessage` 说的是 "your **included API usage**"（对应 `Da`），
`autoModelSelectedDisplayMessage` 说的是 "your **included total usage**"（对应总额）。

四次抓取交叉验证，分母恒定而用量各不相同：

| 抓取 | `apiPercentUsed` | API 用量 | auto 用量 | 相加 | `breakdown.total` |
| --- | --- | --- | --- | --- | --- |
| 1 | 21.025% | 4205 | 485 | 4690 | 4690 ✓ |
| 2 | 24.75% | 4950 | 485 | 5435 | 5435 ✓ |
| 3 | 33.535% | 6707 | 485 | 7192 | 7192 ✓ |
| 4 | 36.04% | 7208 | 485 | 7693 | 7693 ✓ |

**「总分母 = 45 × limit」这个猜测已排除**：`45 × 2000 = 90000` 只是数值巧合，真实结构是
`20000 + 70000` 这个有机制解释（API 池 + auto 池）的拆分。

探测 CLI 的第 5 段会把这套反解结果打印出来。**分母不是硬编码的**，每次都从当前响应
现算，所以换一个额度不同的账号会自动显示那个账号的真实分母。同时会做自洽校验并打印
「闭合 / 不闭合」——因为解方程使得「两路相加等于总量」变成恒等式、不构成独立检验，
真正的独立证据是**解出来的金额必须全部落在整数分上**。一旦 Cursor 改了计费模型，
反解就会出现带小数的脏数字，校验随即报警。

#### 结论

**直接采信服务端的 `totalPercentUsed`。** 自算分支（`used/limit`）只在没有 breakdown、
`limit` 确实覆盖全部额度时才有意义，否则代码返回 nil、显示为「—」。

即便现在已经知道分母是 90000，**代码里依然没有硬编码它**，原因有两条：一是循环依赖
——反解分母必须先有 `totalPercentUsed`，而自算分支存在的意义恰恰是「服务端没给
`totalPercentUsed` 时的兜底」，那种场景下分母同样推不出来；二是 $200/$700 这个拆分
是否随账号套餐变化，目前只有一个账号的观测。

### On-Demand 状态判断

`enabled` 是服务端直接给出的事实，**优先采信**；`limit` 只决定「上限是多少」，
不能拿来推断「开没开」。共有四种互斥状态：

| 状态 | 条件 |
| --- | --- |
| 未开启 | `enabled=false`，或 `enabled` 缺失且没有固定上限 |
| 已开启，有固定上限 | `limit > 0` |
| 已开启，无个人固定上限（上限由团队侧管理） | `enabled=true` + `limitType=team` + `limit=null` |
| 已开启且无固定上限 | `enabled=true` + 非 team + 无 limit |

第三种是 team 账号的常态，早期实现从 `limit` 反推启用状态，把它误判成了「未开启」。

## 实测结论（2026-08-16）

本机账号：`membershipType=enterprise`、`teamMembershipType=SELF_SERVE`、
`isTeamMember=true`、`individualMembershipType=free`、`signUpType=Auth_0`。
**这是一个自助式团队账号**，用量解析会走 `teamUsage` 分支——注意 team 场景和个人账号
的额度口径不一样，不要拿个人账号的假设去套。

### Cookie 构造：多候选对照实验的答案

用量接口需要 Cookie `WorkosCursorSessionToken={userId}%3A%3A{accessToken}`。
五个候选 userId 的实测结果：

| # | 候选 | 结果 |
| --- | --- | --- |
| 1 | `sub` 按 `\|` 切最后一段（Rust 逻辑） | **200 成功** |
| 2 | 完整 `sub`（不切） | **200 成功** |
| 3 | `stripeMembershipAuthId` | 与候选 2 逐字节相同，跳过 |
| 4 | GetUserMeta 的 `workosId` | 与候选 1 逐字节相同，跳过 |
| 5 | 不带 Cookie，仅 `Authorization: Bearer` | **401 失败** |

第 5 路的响应体：

```json
{"error":"not_authenticated","description":"The user does not have an active session or is not authenticated"}
```

**结论：用候选 1（`sub` 按 `|` 切最后一段）。** 它成功、语义正确，而且不像候选 4
那样需要先多打一次 `GetUserMeta`。

### 「`Auth_0` 会导致切不出 `user_` 前缀」——此担忧已排除

早期担心：本机 `signUpType` 是 `Auth_0`、`stripeMembershipAuthId` 是 `auth0|xxx` 形态，
按 `|` 切出来的可能不是 `user_` 开头，从而让 Rust 的提取逻辑失效。

**实测证明这个担忧不成立。** `sub` 的真实结构是：

```
auth0|user_00000000000000000000000000      (37 字符)
      └── 切 '|' 之后 ──> user_00000000000000000000000000   (31 字符，合法 WorkOS ID)
```

`auth0|` 只是 Auth0 的 **provider 前缀**，后面跟的仍然是标准的 WorkOS user ID
（`user_` + 26 位 ULID）。**注册方式（`Auth_0`）与能否切出 `user_` 前缀是正交的**，
以后不用再怀疑这一条。

另外实测确认：`cursorAuth/stripeMembershipAuthId` 与 JWT 的 `sub` **逐字节相同**，
`GetUserMeta.workosId` 与切段结果**逐字节相同**。所以「五路对照」实际只有 3 种不同组合。

### 服务端不校验 Cookie 里的 userId 段

候选 2 传的是 `auth0|user_01KY...`——既不是合法 WorkOS ID，`|` 按 RFC 6265 甚至
**不是 cookie value 的合法字符**——服务端照样返回 200，响应字节数与候选 1 完全一致。
而候选 5（不带 Cookie）明确 401。

**推论：服务端只解析验证 `%3A%3A` 之后的 accessToken，前面的 userId 段不做任何校验。**
因此 Rust 参照实现里「切不出 `user_` 前缀就放弃请求」的前置检查是**过度严格**的；
真遇到切不出来的账号，直接塞完整 `sub` 也能工作，可以作为兜底策略。

## 当前进度

已实测验证（2026-08-16 真机跑通）：

- [x] 只读读取 `state.vscdb` 登录态
- [x] JWT payload 解析（纯标准库）
- [x] GetUserMeta / full_stripe_profile / usage-summary 三类接口
- [x] usage-summary 多候选 Cookie 对照实验（结论见上）
- [x] 用量解析的 camelCase 路径 + team 账号 On-Demand 状态

已实现但**尚无真机覆盖**（只有单元测试，真实响应没走到过这些分支）：

- [x] 用量解析的 snake_case 路径 —— 真实响应全是 camelCase
- [x] `spendLimitUsage` 兜底分支 —— 真实响应里没有这个字段
- [x] `stripe_profile` 回退端点 —— `full_stripe_profile` 一次就成功，没触发回退

进程与账号库（2026-08-17 真机跑通）：

- [x] `Cursor.exe` 三级查找 —— 本机命中第 1 级（HKLM 64 位视图，系统级安装在 D 盘）
- [x] 主进程识别 —— 19 个进程里精确命中 1 个
- [x] 账号库存储层 + `acct save` 从 `state.vscdb` 导出当前账号（对 Cursor 只读）

切号（2026-08-17 完成，**核心需求达成**）：

- [x] `internal/vscdb/write.go` 读写入口，只读函数原样未动
- [x] `internal/switcher` 完整流程：拒绝运行中的 Cursor → 备份旧值 → 事务内写 6 删 2
      → `wal_checkpoint(TRUNCATE)` → 复查 `typeof`
- [x] `acct import` 从 accessToken 或 JSON 导入账号，账号库这才有多个账号可切
- [x] `acct switch -token` 一条命令直切（构造账号 → 存库 → 写库串联复用，不是另写一套），
      易用性追平老版本 Rust/Cockpit 的「一个 token 一条命令」
- [x] `acct switch` / `acct rollback`，18 个单元测试跑在与 Cursor 建表语句相同的临时库上
- [x] CLI 全链路在临时夹具库上实跑通过：切号 → 裸 SQL 核对 → 回滚 → 8 个 key 逐一还原
- [x] 「Cursor 在运行就拒绝」在真机上实测生效（对真库执行 `switch`，在打开数据库**之前**
      就被拦下，真库全程未被写入）

已实现但**尚无真机覆盖**（刻意从未执行）：

- [x] 对**真实** `state.vscdb` 的写入 —— 开发环境就是 Cursor 本身，真写下去会毁掉
      当前会话的登录态。验证一律在临时夹具库上做
- [x] 优雅关闭 → 超时强杀、启动 Cursor（`-kill` / `-start`）——
      执行下去会掐断自己的会话，只做了 dry-run 预演（probe 第 7 段）
- [x] 账号库的多账号场景 —— 真账号库里只有 1 个账号，多账号只在临时库里演练过

未开始：

- [ ] OAuth 登录（有了它才能自动获取新账号的 token，而不是手工粘贴）
- [ ] Wails v3 GUI

## 仍然未知的问题

- **$200 / $700 这个拆分是否随账号套餐变化？** 四次抓取解出的分母完全一致，但都来自
  同一个账号（且 `plan.limit` 四次恒为 2000）。要判断这两个数是常量、还是随套餐/额度
  缩放，需要另一个额度不同的账号做对照。反解逻辑本身不受影响——它每次都现算。
- `stripe_profile` 回退端点始终没被触发过（`full_stripe_profile` 每次都直接成功）。
- 用量解析的 snake_case 路径与 `spendLimitUsage` 分支只有单元测试覆盖，真实响应从未
  走到过；测试数据是按代码期望的形状构造的，真实字段名未必与假设一致。
- Cursor 版本号待确认。
- **切号后 Cursor 是否真的以新账号登录，尚未在真机验证过。** 写入路径本身已在临时库上
  完整覆盖（含 `typeof` 断言），但「Cursor 读到这些值之后的行为」只能等真机切一次才知道。
  可能需要补的：删掉的两个 key 是否足够、`onboardingDate` 缺失会不会触发引导流程。
- 删除 `cachedScopedProfile` / `onboardingDate` 之外，是否还有别处（如 `cursorDiskKV`）
  缓存着账号绑定状态，没有排查过。
