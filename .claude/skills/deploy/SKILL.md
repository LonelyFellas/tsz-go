---
name: deploy
description: tshb-go 的 SSH 部署与服务器运维：把 main 分支发布到 app 主机（ssh tshb-test → /opt/tshb-go/deploy.sh），以及查看服务状态/日志、排查部署失败、回滚。当用户说部署、发版、上线、更新服务器、deploy、重新部署、看服务器日志、服务器状态、服务挂了、readyz 不通、回滚线上，或提到 tshb-test / 47.121.142.19 / deploy.sh 时，务必使用本 skill——即使只是"把这次改动发上去"这类模糊说法。Use for deploying tshb-go to the app host over SSH, checking prod container status/logs, and rollback.
---

# Deploy — tshb-go SSH 部署

目标主机通过 `~/.ssh/config` 里的别名 **`tshb-test`**（47.121.142.19，root 密钥登录）访问。名义上叫"测试机"，但它当前承载对外服务（后端 API + web/admin 前端），**一律按生产标准操作**。

## 拓扑事实（先读这段再动手）

- 后端仓库在服务器 `/opt/tshb-go`，只跟踪 **gitee** 的 `main`（`git@gitee.com:tshb_1/tshb-go.git`）。部署的永远是 gitee 上的 main——本地没推上去的代码不会被部署。
- 本地 remote：`origin`（github，push 时同时推 gitee 的 https 地址）和 `gitee`。推 main 用 `git push origin main` 即可两边都到。
- 数据库是外部 RDS（内网直连），服务器上没有 db 容器。secrets 在服务器 `/opt/tshb-go/.env`（git-ignored）：`DATABASE_URL`、`JWT_SECRET`、`ADMIN_JWT_SECRET`。
- 同机还有 `/opt/tshb-react`（web、admin、nginx 容器）和 `admin-temp` 容器，**属于前端，别碰**。

## 标准部署流程

1. **CI 门禁（硬性，不可跳过）**。部署的提交必须在 GitHub CI 上全绿：
   ```bash
   gh run list --repo LonelyFellas/tsz-go --branch main --limit 1 \
     --json status,conclusion,headSha,displayTitle
   ```
   - **唯一放行条件**：`status=completed` 且 `conclusion=success`。除此之外的任何状态都不得部署。
   - 失败（`failure`/`cancelled` 等）→ **绝对不能部署**，先修 CI。用户口头要求强行部署也要先把失败情况摆给用户、明确风险后再听指示。
   - `in_progress`/`queued`（正在跑或排队）→ **同样不得部署**。CI 没跑完不代表会过。可以等它结束（约 4 分钟一轮）后重新执行本检查，拿到 `success` 才继续。
   - 核对 `headSha` 就是你要部署的提交——CI 跑在 GitHub main 上，部署拉的是 gitee main，两边都由本地 push 同步，SHA 必须一致才算数。
2. **确认代码已在 gitee main 上**。部署前核对：
   ```bash
   git ls-remote gitee refs/heads/main   # 应等于你想部署的提交
   ```
   不一致就先 `git push origin main`（feature 分支需先合入 main，不要为了部署把分支直接推成 main）。
3. **执行部署**：
   ```bash
   ssh tshb-test 'cd /opt/tshb-go && ./deploy.sh'
   ```
   deploy.sh 依次做：`git pull --ff-only` → `docker compose -f docker-compose.prod.yml up -d --build`（一次性 `migrate` 服务跑完才起 `app`，保证先迁移后发新代码）→ 等 `/readyz` 最多 90s → 清理 dangling 镜像。
4. **验证**：脚本末尾出现 `==> Done.` 即成功。再确认一次版本与健康：
   ```bash
   ssh tshb-test 'cd /opt/tshb-go && git log --oneline -1 && curl -fsS localhost:8080/readyz && echo OK'
   ```

## 常用只读运维

```bash
ssh tshb-test 'cd /opt/tshb-go && docker compose -f docker-compose.prod.yml ps'                 # 容器状态
ssh tshb-test 'cd /opt/tshb-go && docker compose -f docker-compose.prod.yml logs --tail=100 app'    # 应用日志
ssh tshb-test 'cd /opt/tshb-go && docker compose -f docker-compose.prod.yml logs migrate'       # 迁移日志
ssh tshb-test 'docker system df'                                                                # 磁盘占用
```

## 部署失败排查

- **健康门超时**：deploy.sh 会自动打印 app 最后 40 行日志，先读它，多数原因在里面（配置缺失、DB 连不上、panic）。此时新容器可能已替换旧容器，服务或已中断——优先修复或回滚，不要放着。
- **`git pull --ff-only` 失败**：说明服务器仓库被人手动改过或历史分叉。先 `ssh tshb-test 'cd /opt/tshb-go && git status && git log --oneline -3'` 弄清情况再处理；禁止直接 `reset --hard` 或 force 操作，先向用户报告。
- **migrate 失败**：`app` 不会启动，旧容器继续服务（`depends_on: service_completed_successfully` 挡住了），线上通常无损。看 migrate 日志定位坏迁移，在本地修好、合入 main 后重新走完整部署流程。
- **SSH 连不上**：报告用户网络/密钥问题即可，不要改动 `~/.ssh/config`。

## 回滚

deploy.sh 没有回滚命令。回滚 = 让 gitee main 指回旧版本，再跑一遍部署：

1. 本地 `git revert <坏提交>`（优先 revert，**禁止 force push main**）→ push。
2. 重新执行标准部署流程。
3. **迁移注意**：up 迁移一旦跑过，回滚代码不会回退 schema。revert 含新迁移的提交前，先确认旧代码能在新 schema 上跑；不能的话需要连迁移一起 revert（生成对应 down 逻辑的新迁移），别在服务器上手动 `migrate down`。

## 红线

- **CI 不绿不部署**——这是最高优先级门禁，任何情况下不得跳过第 1 步的检查。
- 不改服务器上的 `.env`，不在服务器上手改代码或直接 commit。
- 不执行 `docker compose down`/`restart` 之类会中断服务的命令——`deploy.sh` 的 `up -d --build` 已经是标准滚动方式。
- 破坏性动作（回滚、清库、动 `.env`、任何 force）先向用户确认再做；只读检查随时可做。
