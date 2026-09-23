# lite-api

面向企业和团队内部使用的 AI 网关，使用 Go、PostgreSQL 和 Redis，单实例部署。管理员创建账户，用户管理自己的 API Key。内置前端目录保留占位，当前开发后端。

目前已实现空库初始化、结构校验、首个管理员初始化、密码登录与令牌刷新/撤销、用户管理、分组基础配置/授权、用户 API Key 管理及管理员余额调整、上游 API Key 账号与代理管理、文本手动测试和定时测试计划。客户端网关转发、计费、媒体、模型广场及扩展运行功能仍在开发中。

## 本地运行

需要 Go 1.25.4 或更新的兼容版本、Docker 和 Compose。

```sh
cp .env.example .env
# 编辑 .env，填写 JWT_SECRET（至少 32 字节随机值）和首个管理员凭证。
docker --context desktop-linux compose up -d
set -a
. ./.env
set +a
go build -o bin/lite-api ./cmd/lite-api
./bin/lite-api init-db
./bin/lite-api bootstrap
./bin/lite-api serve
```

`init-db` 只接受空库，`bootstrap` 只接受没有用户的已初始化数据库。`serve` 只检查结构，不执行迁移。初始化后可从运行环境移除 `ADMIN_EMAIL`、`ADMIN_PASSWORD`。数据库与 Redis 端口仅绑定本机，示例数据库密码只用于本地验证；部署时使用独立凭证及 HTTPS 反向代理。

用户/管理 API 使用 `{code,message,data}` 响应格式。登录为 `POST /api/v1/auth/login`（`email`、`password`）；其他管理请求使用返回的 Bearer access token。access token 有效期 15 分钟，会话最多 30 天；refresh token 每次刷新后失效。客户端 API Key 不能用作管理面登录凭证。管理员余额调整可通过 `Idempotency-Key` 防止重复提交，金额计算在 PostgreSQL NUMERIC 中完成。

## 上游与健康测试

管理员通过 `/api/v1/admin/accounts` 配置 `platform`、`type=apikey`、`credentials.api_key`、可选 `credentials.base_url` 和 `group_ids`。支持的账号平台为 openai、anthropic、gemini、grok、kimi、zhipu、deepseek、minimax；仅接受按量 API Key，账号查询不会返回原始 Key。`POST /api/v1/admin/accounts/{id}/test` 使用 `model_id` 发起真实文本请求，返回测试 SSE；它可能产生上游费用，不计入内部用户消费。

`/api/v1/admin/scheduled-test-plans` 支持创建、编辑、删除，`/{id}/results` 查询结果，`/api/v1/admin/accounts/{id}/scheduled-test-plans` 列出账号计划。这组接口沿用直接 JSON 响应。五字段 cron 默认 UTC，可用 `CRON_TZ=Asia/Shanghai` 指定时区。单实例每 15 秒扫描，按到期顺序执行，每次调用最多 45 秒；`max_results` 控制留存，`auto_recover` 仅恢复可恢复状态，保留人工停调和消费额度。进程停止会取消正在执行的请求，未完成计划下次启动继续检查。

代理支持 HTTP、HTTPS、SOCKS5/SOCKS5h 和到期后的明确备用/直连配置。上游重定向不跟随，TLS 验证不能关闭，公开上游必须使用 HTTPS。内网模型或代理须在部署环境 `UPSTREAM_PRIVATE_CIDRS` 中明确允许目标网段；默认拒绝本机和私网。不要将该配置扩大为任意地址。

独立上游验证无需数据库：设置本地 `UPSTREAM_BASE_URL`、`UPSTREAM_API_KEY`、`UPSTREAM_MODEL` 后执行 `./bin/lite-api upstream-check`。命令发起一次 JSON 和一次 SSE 文本生成，仅输出状态和时延摘要。Go 客户端对指定中转和 `gpt-5.6-luna` 的 JSON/SSE 已实测成功；其他平台当前由协议模拟测试覆盖，不能据此宣称所有原厂模型均已联调。

## 验证

```sh
go test ./...
go vet ./...
DOCKER_CONTEXT=desktop-linux python3 scripts/check-schema.py
DOCKER_CONTEXT=desktop-linux python3 scripts/test-integration.py
```

集成脚本创建独立的临时 PostgreSQL/Redis，运行 race 检测和真实数据库测试，结束后删除测试容器。普通 `go test` 未配置 `TEST_DATABASE_URL`、`TEST_REDIS_URL` 时跳过数据库集成用例。

数据库定义位于 `schema/baseline.sql`，固定来源及校验和位于 `schema/source.json`。新库省略两张插件表及其专属对象，其余业务表保留原结构和含义。`schema/contract.json` 固定表列、约束、索引、函数、触发器和序列定义；结构改变会拒绝启动。确需变更基线时，审核 SQL 后以 `scripts/check-schema.py --write-contract` 重新生成契约。正常运行不依赖其他项目目录。

`reserve/` 是被 Git 忽略的本地规划目录。来源版权及许可证见 `NOTICE`、`LICENSE`、`COPYING`。
