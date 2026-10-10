# sub2api 项目开发指南

> 本文档记录项目环境配置、常见坑点和注意事项，供 Claude Code 和团队成员参考。

## 一、项目基本信息

| 项目 | 说明 |
|------|------|
| **上游仓库** | Wei-Shaw/sub2api |
| **Fork 仓库** | zoutao212/sub2api（本地 remote 名 `myfork`；`origin` 指向原作者 Wei-Shaw/sub2api） |
| **技术栈** | Go 后端 (Ent ORM + Gin) + Vue3 前端 (pnpm) |
| **数据库** | PostgreSQL 16 + Redis |
| **包管理** | 后端: go modules, 前端: **pnpm**（不是 npm） |
| **构建/部署分支** | **`custom`**（不是 main——main 是上游纯净镜像，详见第四章） |

## 二、本地环境配置

### PostgreSQL 18 (Windows 服务)

> 2026-09 本机实况：已从 PG 16 升级为 PostgreSQL 18，端口为 **5433**（非默认 5432）。

| 配置项 | 值 |
|--------|-----|
| 服务名 | `postgresql-x64-18` |
| 端口 | **5433** |
| psql 路径 | `C:\Program Files\PostgreSQL\18\bin\psql.exe` |
| pg_hba.conf | `C:\Program Files\PostgreSQL\18\data\pg_hba.conf` |
| 超级用户 | user=`postgres`（密码由主人自持，与旧笔记 `postgres/postgres` 不同） |
| 数据库 | `sub2api`（由 setup 向导自动建库+跑迁移；未创建独立 sub2api 用户，直接用 postgres 连接） |
| 监听特性 | 仅监听 IPv6 `[::]:5433`；连接必须用 `localhost` 或 `::1`，填 `127.0.0.1` 会被拒绝 |

### Redis（Memurai Developer 4.1.8）

> 2026-09 安装：Windows 原生 Redis（Memurai Developer 免费版），注册为系统服务，开机自启。

| 配置项 | 值 |
|--------|-----|
| 服务名 | `Memurai`（`sc query Memurai` 查看状态） |
| 监听 | `127.0.0.1:6379`（IPv4） |
| 密码 | 无 |
| 注意 | Developer 版每 10 天需重启一次服务；生产使用需 Enterprise 版 |
| 安装方式 | `choco install memurai-developer -y`（需管理员提权） |

### sub2api 服务运行方式（Win11 本机）

| 项 | 值/说明 |
|----|---------|
| 二进制 | `backend/sub2api.exe`（`go build -tags embed -o sub2api.exe ./cmd/server` 生成，已内嵌前端） |
| 运行目录 | `backend/`（`config.yaml` 与 `.installed` 都相对运行目录） |
| 配置文件 | `backend/config.yaml`（由 setup 向导自动生成；数据库 `localhost:5433`、Redis `127.0.0.1:6379`） |
| 启动方式 | 用独立进程方式启动（见坑 12），日志重定向到文件 |
| 管理员 | 邮箱 `zoutao212@gmail.com`（密码由主人自持；首次登录需在控制台完成“部署与运营合规确认”） |
| 前端产物 | `backend/internal/web/dist/`（pnpm build 输出，不入 git） |

**一键启动脚本（项目根目录）**：

- 双击 `start-sub2api.bat`：自动检查/启动依赖服务（PG/Redis 未运行时自动拉起，必要时弹一次 UAC）→ 以隐藏窗口方式独立启动 sub2api（WMI，零黑窗）→ 就绪后自动打开浏览器 → 启动器窗口自动关闭
- 双击 `stop-sub2api.bat`：停止 sub2api（PostgreSQL / Redis 系统服务保持运行）
- 实现要点：见坑 12 的 2026-09 补充（CONFIG_FILE / DATA_DIR 环境变量）
- 脚本编码为 **GBK + CRLF**（中文 Windows cmd 兼容；直接以 UTF-8 保存会因解析错位随机报错）。修改流程：`iconv -f GBK -t UTF-8` 转出编辑 → 行尾 `sed -i 's/\r*$/\r/'` → `iconv -f UTF-8 -t GBK` 转回

### 开发工具

```bash
# golangci-lint（CI 用 v2.13，本地建议装同一版以免版本差异带来的噪音）
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13

# pnpm (前端包管理)
npm install -g pnpm
```

## 三、CI/CD 流水线

### GitHub Actions Workflows

| Workflow | 触发条件 | 检查内容 |
|----------|----------|----------|
| **backend-ci.yml** | push, pull_request | 单元测试 + 集成测试 + golangci-lint v2.13 |
| **security-scan.yml** | push, pull_request, 每周一 | govulncheck + gosec + pnpm audit |
| **release.yml** | tag `v*` | 构建发布（PR 不触发） |

### CI 要求

- Go 版本必须是 **1.27.0**：三个 workflow 都用 `go-version-file: backend/go.mod` 取版本，随后硬断言 `go version | grep -q 'go1.27.0'`。升级 Go 时要同时改 `backend/go.mod`、`backend-ci.yml`（两处）、`release.yml`、`security-scan.yml` 里的这句断言，**以及三个 Dockerfile 里的 Go 构建镜像**（`Dockerfile` / `deploy/Dockerfile` 的 `ARG GOLANG_IMAGE`、`backend/Dockerfile` 的 `FROM golang:`）。前者漏了 CI 会在版本校验步骤直接失败；**后者漏了 CI 不会报，而是等到有人用这些 Dockerfile 构建时才失败**（`go.mod requires go >= X (running Y; GOTOOLCHAIN=local)`）。
- 前端使用 `pnpm install --frozen-lockfile`，必须提交 `pnpm-lock.yaml`

### 本地测试命令

```bash
# 后端单元测试
cd backend && go test -tags=unit ./...

# 后端集成测试
cd backend && go test -tags=integration ./...

# 代码质量检查
cd backend && golangci-lint run ./...

# 前端依赖安装（必须用 pnpm）
cd frontend && pnpm install
```

## 四、Git 分支模型与上游同步流程

### 分支模型（2026-10-09 起）

| 分支 | 角色 | 规则 |
|------|------|------|
| `main` | 上游纯净镜像 | **禁止**携带任何本地提交。只要 main 有独有提交，GitHub 的 Sync fork 按钮就会永久失效（显示 "This branch has conflicts"）。 |
| `custom` | 上游 main + 全部本地定制 | **构建、部署、运行都用这个分支。** |

本地 remote 命名（注意与常规 fork 布局相反，是刻意为之）：

- `origin` → `Wei-Shaw/sub2api`（原作者）
- `myfork` → `zoutao212/sub2api`（自己的 fork）

### 日常同步：双击 sync-upstream.bat

仓库根目录的 `sync-upstream.bat`（本地专用，不入库）一键完成四步：

```
git fetch origin --prune && git fetch myfork --prune
git checkout main && git merge --ff-only origin/main && git push myfork main
git checkout custom && git merge origin/main
git push myfork custom
```

前 3 步都是快进或普通 merge，**没有任何 force push**，可重复运行。冲突只在 `custom` 分支出现，脚本会停下并提示收尾命令。

### 解冲突时怎么分清"谁的锅"

`custom_changes.json`（入库于 custom 分支）记录了每份定制相对上游的**精确行段**：

```json
{
  "id": "CHG-005",
  "file": "frontend/src/components/account/AccountUsageCell.vue",
  "added_line_ranges_vs_upstream_main": ["299-310", "312-321", "..."],
  "hunk_count": 6
}
```

冲突落在某文件时，先查这个文件在清单里的行段：区间内是本地定制，区间外是上游改动。

### 本地专用文件（不入库，因此永不冲突）

通过 `.git/info/exclude` 排除，不污染仓库 `.gitignore`，也不会误传给上游：

- `DEV_GUIDE.local.md` —— 本文档。**已从上游的 `DEV_GUIDE.md` 改名而来**，避免与上游同名文件对撞
- `start-sub2api.bat` / `stop-sub2api.bat` —— 启动脚本，GBK + CRLF
- `sync-upstream.bat` —— 本同步脚本

⚠️ 这些文件只存在于本机磁盘。若重建仓库，可从 `custom` 分支的历史恢复 bat 与本文档；`sync-upstream.bat` 需按本章内容重写。

---

## 五、常见坑点 & 解决方案

### 坑 1：pnpm-lock.yaml 必须同步提交

**问题**：`package.json` 新增依赖后，CI 的 `pnpm install --frozen-lockfile` 失败。

**原因**：上游 CI 使用 pnpm，lock 文件不同步会报错。

**解决**：
```bash
cd frontend
pnpm install  # 更新 pnpm-lock.yaml
git add pnpm-lock.yaml
git commit -m "chore: update pnpm-lock.yaml"
```

---

### 坑 2：npm 和 pnpm 的 node_modules 冲突

**问题**：之前用 npm 装过 `node_modules`，pnpm install 报 `EPERM` 错误。

**解决**：
```bash
cd frontend
rm -rf node_modules  # 或 PowerShell: Remove-Item -Recurse -Force node_modules
pnpm install
```

---

### 坑 3：PowerShell 中 bcrypt hash 的 `$` 被转义

**问题**：bcrypt hash 格式如 `$2a$10$xxx...`，PowerShell 把 `$2a` 当变量解析，导致数据丢失。

**解决**：将 SQL 写入文件，用 `psql -f` 执行：
```bash
# 错误示范（PowerShell 会吃掉 $）
psql -c "INSERT INTO users ... VALUES ('$2a$10$...')"

# 正确做法
echo "INSERT INTO users ... VALUES ('\$2a\$10\$...')" > temp.sql
psql -U sub2api -h 127.0.0.1 -d sub2api -f temp.sql
```

---

### 坑 4：psql 不支持中文路径

**问题**：`psql -f "D:\中文路径\file.sql"` 报错找不到文件。

**解决**：复制到纯英文路径再执行：
```bash
cp "D:\中文路径\file.sql" "C:\temp.sql"
psql -f "C:\temp.sql"
```

---

### 坑 5：PostgreSQL 密码重置流程

**场景**：忘记 PostgreSQL 密码。

**步骤**：
1. 修改 `C:\Program Files\PostgreSQL\16\data\pg_hba.conf`
   ```
   # 将 scram-sha-256 改为 trust
   host    all    all    127.0.0.1/32    trust
   ```
2. 重启 PostgreSQL 服务
   ```powershell
   Restart-Service postgresql-x64-16
   ```
3. 无密码登录并重置
   ```bash
   psql -U postgres -h 127.0.0.1
   ALTER USER sub2api WITH PASSWORD 'sub2api';
   ALTER USER postgres WITH PASSWORD 'postgres';
   ```
4. 改回 `scram-sha-256` 并重启

---

### 坑 6：Go interface 新增方法后 test stub 必须补全

**问题**：给 interface 新增方法后，编译报错 `does not implement interface (missing method XXX)`。

**原因**：所有测试文件中实现该 interface 的 stub/mock 都必须补上新方法。

**解决**：
```bash
# 搜索所有实现该 interface 的 struct
cd backend
grep -r "type.*Stub.*struct" internal/
grep -r "type.*Mock.*struct" internal/

# 逐一补全新方法
```

---

### 坑 7：Windows 上 psql 连 localhost 的 IPv6 问题

**问题**：psql 连 `localhost` 先尝试 IPv6 (::1)，可能报错后再回退 IPv4。

**建议**：直接用 `127.0.0.1` 代替 `localhost`。

---

### 坑 8：Windows 没有 make 命令

**问题**：CI 里用 `make test-unit`，本地 Windows 没有 make。

**解决**：直接用 Makefile 里的原始命令：
```bash
# 代替 make test-unit
go test -tags=unit ./...

# 代替 make test-integration
go test -tags=integration ./...
```

---

### 坑 9：Ent Schema 修改后必须重新生成

**问题**：修改 `ent/schema/*.go` 后，代码不生效。

**解决**：
```bash
cd backend
go generate ./ent  # 重新生成 ent 代码（json.RawMessage 字段会生成为同类型的 jsontext.Value，属预期）
git add ent/       # 生成的文件也要提交
```

---

### 坑 10：前端测试看似正常，但后端调用失败（模型映射被批量误改）

**典型现象**：
- 前端按钮点测看起来正常；
- 实际通过 API/客户端调用时返回 `Service temporarily unavailable` 或提示无可用账号；
- 常见于 OpenAI 账号（例如 Codex 模型）在批量修改后突然不可用。

**根因**：
- OpenAI 账号编辑页默认不显式展示映射规则，容易让人误以为“没映射也没关系”；
- 但在**批量修改同时选中不同平台账号**（OpenAI + Antigravity/Gemini）时，模型白名单/映射可能被跨平台策略覆盖；
- 结果是 OpenAI 账号的关键模型映射丢失或被改坏，后端选不到可用账号。

**修复方案（按优先级）**：
1. **快速修复（推荐）**：在批量修改中补回正确的透传映射（例如 `gpt-5.3-codex -> gpt-5.3-codex-spark`）。
2. **彻底重建**：删除并重新添加全部相关账号（最稳但成本高）。

**关键经验**：
- 如果某模型已被软件内置默认映射覆盖，通常不需要额外再加透传；
- 但当上游模型更新快于本仓库默认映射时，**手动批量添加透传映射**是最简单、最低风险的临时兜底方案；
- 批量操作前尽量按平台分组，不要混选不同平台账号。

---

### 坑 11：pnpm 12 会重写 lockfile 并生成 pnpm-workspace.yaml

**问题**：本机 pnpm 12 执行 `pnpm install` 后，`frontend/pnpm-lock.yaml` 被重写（上游 CI 用 pnpm 9 + `--frozen-lockfile` 校验），且多出未跟踪的 `frontend/pnpm-workspace.yaml`。

**解决**：
```bash
# 还原 lockfile、删除多余文件（不提交这类本地生成物）
git restore -- frontend/pnpm-lock.yaml
rm frontend/pnpm-workspace.yaml
```

---

### 坑 12：本机启动常驻服务进程会让终端命令挂起

**问题**：在 Git Bash 中用带输出重定向的 `Start-Process`（或 `&` 后台符）启动常驻进程（如 sub2api.exe）时，子进程继承调用链句柄，导致该条终端命令永不结束、后续命令全部排队卡死（2026-09 实测：向导进程把整条终端命令队列堵死，需人工杀进程才能解锁）。

**解决**：用 WMI 启动（完全脱离调用会话，零句柄继承）：
```bash
powershell -NoProfile -Command "([wmiclass]'Win32_Process').Create('cmd /c set CONFIG_FILE=e:\path\backend\config.yaml&& set DATA_DIR=e:\path\backend&& e:\path\backend\sub2api.exe > e:\path\run.log 2>&1')"
```
停止服务：`taskkill //F //IM sub2api.exe`（Git Bash 中 `/F` 须写成 `//F` 防路径转换）。

**2026-09 补充（重要）**：WMI 启动必须显式设置两个环境变量：

- `CONFIG_FILE=<backend 绝对路径>\config.yaml`：viper 配置加载优先读取它；
- `DATA_DIR=<backend 绝对路径>`：`setup.NeedsSetup()` 用来定位 config.yaml 与 .installed。

实测：不设置时，WMI 进程的工作目录与预期不一致，`NeedsSetup()` 误判“未安装”，服务静默进入 setup 向导模式（只注册 `/setup/*` 路由，正常业务 API 全 404）。

---

### 坑 13：PowerShell 里没有 pnpm，验证命令要直接调 node_modules/.bin

**问题**：`package.json` 里的 `scripts` 全部以 `pnpm` 开头（如 `build` = `pnpm run check:i18n && vue-tsc -b && vite build`），但本机 PowerShell 会话的 PATH 中没有 `pnpm`（`where pnpm` 返回未找到，只有 `node` / `npm`）。直接跑 `pnpm run check:i18n` 会报“术语 'pnpm' 不会被识别”，且 PowerShell 下 `$LASTEXITCODE` 仍为 0，**极易被误判为“检查通过”**。

**解决**：绕过包管理器，直接调用 `frontend/node_modules/.bin` 下的可执行入口（PowerShell 需带 `.cmd` 后缀）：

```powershell
cd frontend
& ".\node_modules\.bin\vitest.cmd" run src/i18n/__tests__/localeKeyCompleteness.spec.ts
& ".\node_modules\.bin\vue-tsc.cmd" --noEmit
```

对照关系：`pnpm run check:i18n` → `vitest run src/i18n/__tests__/localeKeyCompleteness.spec.ts`；`pnpm typecheck` → `vue-tsc --noEmit`。

若要彻底解决，执行 `npm install -g pnpm`（或用 corepack）后重启终端。

---

### 坑 14：PR 提交前检查清单

提交 PR 前务必本地验证：

- [ ] `go test -tags=unit ./...` 通过
- [ ] `go test -tags=integration ./...` 通过
- [ ] `golangci-lint run ./...` 无新增问题
- [ ] `pnpm-lock.yaml` 已同步（如果改了 package.json）
- [ ] 所有 test stub 补全新接口方法（如果改了 interface）
- [ ] Ent 生成的代码已提交（如果改了 schema）

### 坑 15：文件移出 git 跟踪后，core.autocrlf 不再生效（.bat 会退化成 LF）

**问题**：本仓库 `core.autocrlf=true`，且 `.gitattributes` 只为 `.go/.ts/.vue/.sh/.yaml/.sql` 指定了 `eol=lf`，**没有 `.bat` 规则**。所以当 `start-sub2api.bat` 等脚本被 git 跟踪时，git checkout 会自动把仓库里的 LF blob 转成 CRLF 写回磁盘，Windows cmd 能正常工作。一旦执行 `git rm --cached` 把它们移出跟踪，这层转换就消失了，文件退化为裸 LF——而 cmd 对 LF 换行的 `.bat` 在 `goto`/标签处行为不可靠。

同理，`git reset --hard` 会连带删除"已 `rm --cached`、但旧 HEAD 树里仍存在"的路径，本机曾因此丢失过这两个脚本（已从 `55f0e8807` 按字节恢复）。

**实测检查命令**（裸 LF 计数必须为 0）：

```powershell
python -c "from pathlib import Path; b=Path('start-sub2api.bat').read_bytes(); print('CRLF=',b.count(b'\r\n'),' bareLF=',b.count(b'\n')-b.count(b'\r\n'))"
```

**修复**（幂等，可反复执行）：

```powershell
python -c "from pathlib import Path; [Path(f).write_bytes(Path(f).read_bytes().replace(b'\r\n',b'\n').replace(b'\n',b'\r\n')) for f in ['start-sub2api.bat','stop-sub2api.bat','sync-upstream.bat']]"
```

⚠️ 上述操作请用 Python 而非 PowerShell 重定向（`>` 会按 UTF-8 重写，直接破坏 GBK 中文）。若某天这些脚本被误 `git add`，重新 untrack 后务必再跑一遍换行修复。

---

### 坑 16：同步上游后 Go 会自动下载新工具链（大体积、依赖网络）

**问题**：本机 `go version` 是 **1.26.2**，但 `backend/go.mod` 要求 `go 1.27.x`。因为 `GOTOOLCHAIN=auto`，Go 会自动下载并改用所需工具链，缓存在：

```
C:\Users\zouta\go\pkg\mod\golang.org\toolchain@v0.0.1-go<版本>.windows-amd64
```

2026-10 上游已把 `go.mod` 从 `1.27.0` 升到 **`1.27.2`**，而本机缓存最大只到 `go1.27.0`。所以同步后首次 `go build` 会触发一次约 70–100 MB 的工具链下载；网络受限或代理异常时会直接报 `toolchain not available`，且报错信息不会明确指向"网络问题"，容易误判。

**同步前自检**：

```powershell
go env GOTOOLCHAIN                    # 必须是 auto
cd backend; go list -m -f "{{.GoVersion}}"   # 看 go.mod 实际要求的版本
```

**已缓存版本**（截至 2026-10-09）：`go1.24.11`、`go1.25.8`、`go1.25.9`、`go1.26.0`、`go1.27.0`。

**处置**：确认网络可达后先跑一次 `cd backend; go build ./...`，让工具链下载完成，再做其他验证。若下载失败，改用显式安装：`go install golang.org/dl/go1.27.2@latest` 然后 `go1.27.2 download`。

---

### 坑 17：上游平台架构重构带来数据库迁移

**问题**：上游 2026-10 的 17 个提交是一次"平台清单驱动"大重构（接入 Cline / Command Code，平台列表改由 profile 派生，平台白名单从数据库 CHECK 约束改为应用层校验）。随之带来：

- 新增迁移 `backend/migrations/242_drop_platform_check_constraints.sql`
- Ent schema 变更：`composite_model_route.go`、`user_platform_quota.go`

**影响**：合并上游后必须让应用跑一遍迁移，否则新旧校验逻辑不一致。迁移是 DROP CHECK CONSTRAINT（放宽约束、不删数据），风险等级低，但**仍建议先备份数据库**。

**预防**：本机 PG18@5433 迁移前先 dump：

```powershell
& "C:\Program Files\PostgreSQL\18\bin\pg_dump.exe" -h localhost -p 5433 -U <user> -Fc -f D:\backup\sub2api-pre-sync.dump <dbname>
```

⚠️ 若本地无 `pg_dump` 在 PATH，用完整路径调用（PostgreSQL 的 bin 不默认入 PATH）。

---

### 坑 18：编译 exe 必须加 `-tags embed`，否则前端全丢

**问题**：`backend/internal/web/` 下有两个互斥文件——`embed_on.go`（`//go:build embed`，含 `//go:embed all:dist`）与 `embed_off.go`（`//go:build !embed`，仅 stub）。**不带 tag 编译出来的 exe 不含任何前端资源**，页面直接打不开。

**陷阱**：`backend/Makefile` 里的 `build` 目标**并没有** `-tags embed`，它产出的是前后端分离的部署产物。照抄 Makefile 会得到一个"能启动但没有 UI"的 exe。

**正确命令**（Windows，无 make）：

```bat
cd frontend
call .\node_modules\.bin\vite.cmd build          :: 输出到 backend\internal\web\dist
cd ..\backend
set CGO_ENABLED=0
go build -tags embed -trimpath -o sub2api.exe ./cmd/server
```

要点：

- `vite.config.ts` 的 `outDir: '../backend/internal/web/dist'`、`emptyOutDir: true` —— 前端产物**直接写进 embed 目录**，不需要手工拷贝；
- `vite.config.ts` 挂了 `checker({ vueTsc: true })`，所以 `vite build` 自带类型检查，类型错误会直接中断构建；
- `emptyOutDir: true` 意味着**构建失败会留下不完整的 dist**，所以必须"先确认 dist 完整再编译 Go"，顺序反了会得到一个缺资源的 exe；
- 前端 dist 与后端代码一起嵌进同一个二进制 —— 所以**只 `git pull` 永远不会让改动生效**，必须重新编译；
- Windows 编译时 exe 正被运行会因文件锁失败，必须先 `taskkill`。

一键流程见根目录 `rebuild-sub2api.bat`（含预检、停服、备份、替换、重启）。

---

### 坑 19：版本号与"有新版本可用"提示永远差一格（上游自身问题）

**问题**：页面显示"当前版本 v0.2.5，最新版本 v0.2.15"。查证后发现这是**上游的版本时序问题，不是本机问题**：

| tag | tag 里的 `VERSION` 文件 |
|----|------------------------|
| v0.2.12 | 0.2.11 |
| v0.2.13 | 0.2.12 |
| v0.2.14 | 0.2.13 |
| v0.2.15 | 0.2.14 |

上游每个 release tag 打好之后才更新 `VERSION` 文件，**系统性滞后一位**。

**版本号的真实来源**（三者并存，容易搞混）：

1. `backend/cmd/server/VERSION` —— 被 `//go:embed VERSION` 编译进二进制，**这是页面实际显示的值**；
2. Makefile 的 `-X main.Version=$(VERSION)` —— 由 `scripts/resolve-version.sh` 计算，但**会被第 1 项在 init 时覆盖**，等于无效；
3. `resolve-version.sh` 仅在"恰好检出某个 tag"时才用 tag 名，否则回落到第 1 项。

**结论**：只要同步到 `main`（其 `VERSION` 已是 `0.2.15`）再重新编译，页面就会正确显示 `0.2.15`。**不要为了追 tag 去 checkout v0.2.15** —— 那个 tag 编出来反而是 `0.2.14`。源码构建应始终跟随 `main`。

⚠️ 若页面积累过旧 bundle，版本显示可能滞后于真实值，此时按 Ctrl+F5 强刷。

---

## 六、常用命令速查

### 数据库操作

```bash
# 连接数据库（PG18@5433：必须用 localhost/IPv6，不能用 127.0.0.1）
"C:\Program Files\PostgreSQL\18\bin\psql.exe" -U postgres -h localhost -p 5433 -d sub2api

# 查看所有角色
"C:\Program Files\PostgreSQL\18\bin\psql.exe" -U postgres -h localhost -p 5433 -c "\du"

# 查看所有数据库
"C:\Program Files\PostgreSQL\18\bin\psql.exe" -U postgres -h localhost -p 5433 -c "\l"

# 执行 SQL 文件
"C:\Program Files\PostgreSQL\18\bin\psql.exe" -U postgres -h localhost -p 5433 -d sub2api -f migration.sql
```

### Git 操作

> remote 命名注意：`origin` = 原作者上游，`myfork` = 自己的 fork（不是常规布局，别照抄网上教程）。

```bash
# 同步上游（最常用：直接跑 sync-upstream.bat 即可自动完成下面四步）
git fetch origin --prune
git checkout main
git merge --ff-only origin/main
git push myfork main
git checkout custom
git merge origin/main        # 冲突只在这里出现
git push myfork custom

# 从最新 custom 切功能分支
git checkout custom && git pull myfork custom
git checkout -b feature/xxx

# 让功能分支跟上上游
git fetch origin
git merge origin/main        # 在功能分支上合并，不要 rebase 到共享分支
```

⚠️ 永远不要在 `main` 上提交本地改动，否则 GitHub 的 Sync fork 会失效。

### 前端操作

```bash
# 安装依赖（必须用 pnpm）
cd frontend
pnpm install

# 开发服务器
pnpm dev

# 构建
pnpm build
```

### 后端操作

```bash
# 运行服务器
cd backend
go run ./cmd/server/

# 生成 Ent 代码
go generate ./ent

# 运行测试
go test -tags=unit ./...
go test -tags=integration ./...

# Lint 检查
golangci-lint run ./...
```

## 七、项目结构速览

```
sub2api-bmai/
├── backend/
│   ├── cmd/server/          # 主程序入口
│   ├── ent/                 # Ent ORM 生成代码
│   │   └── schema/          # 数据库 Schema 定义
│   ├── internal/
│   │   ├── handler/         # HTTP 处理器
│   │   ├── service/         # 业务逻辑
│   │   ├── repository/      # 数据访问层
│   │   └── server/          # 服务器配置
│   ├── migrations/          # 数据库迁移脚本
│   └── config.yaml          # 配置文件
├── frontend/
│   ├── src/
│   │   ├── api/             # API 调用
│   │   ├── components/      # Vue 组件
│   │   ├── views/           # 页面视图
│   │   ├── types/           # TypeScript 类型
│   │   └── i18n/            # 国际化
│   ├── package.json         # 依赖配置
│   └── pnpm-lock.yaml       # pnpm 锁文件（必须提交）
└── .claude/
    └── CLAUDE.md            # 本文档
```

## 八、参考资源

- [上游仓库](https://github.com/Wei-Shaw/sub2api)
- [Ent 文档](https://entgo.io/docs/getting-started)
- [Vue3 文档](https://vuejs.org/)
- [pnpm 文档](https://pnpm.io/)
