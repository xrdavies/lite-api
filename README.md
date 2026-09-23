# lite-api

面向企业和团队内部使用的 AI 网关，使用 Go、PostgreSQL 和 Redis，单实例部署。管理员创建账户，用户管理自己的 API Key。内置前端目录保留占位，当前开发后端。

目前已实现空库初始化、结构校验、首个管理员初始化、密码登录与令牌刷新/撤销、用户管理、分组基础配置/授权、用户 API Key 管理及管理员余额调整、上游 API Key 账号与代理管理、文本手动测试和定时测试计划、渠道价格配置和可用渠道查询。已接入 Chat Completions、Anthropic Messages 和 Gemini 原生 JSON/SSE 网关、token 计数、用量与事务扣费、平台额度和基础查询；Responses、协议转换、媒体、完整模型广场及扩展运行功能仍在开发中。

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

## 渠道与定价

管理员通过 `/api/v1/admin/channels` 管理渠道、分组关联、模型映射、价格和账号成本规则。一个分组只能属于一个渠道；关联与价格替换在同一事务中完成。价格沿用各字段的十进制精度，token 价格单位为 USD/token，支持科学计数法。`billing_model_source` 可配置 requested、channel_mapped、upstream 或 response_model。Chat Completions 已使用请求开始时的价格快照结算，消费期间修改价格不会回改该次费用。

价格支持 token、per_request、image，缓存读写及 1h 写入价、上下文阶梯、时段/服务等级/推理倍率。上下文阶梯按 `(min_tokens, max_tokens]` 匹配；时段使用显式时区与 `[start_time, end_time)`，结束 `00:00` 表示当天结束。账号成本规则单独保存，不改变用户价格。配置缺失与显式零价有不同含义，尚无全局参考价目录回落。

`PUT /api/v1/admin/settings` 目前支持 `site_name` 和 `available_channels_enabled`。后者默认关闭，开启后用户可通过 `/api/v1/channels/available` 查询可访问分组下的具体模型和价格，包含映射别名和本人倍率；私有分组、其他平台模型、内部账号成本规则及映射目标不向无权限用户返回。模型广场完整能力继续开发。

## 网关、用量与账务

客户端用自己创建的 Key 作为 Bearer 凭证调用 `POST /v1/chat/completions`，或兼容入口 `/chat/completions`、`/backend-api/codex/chat/completions`。Chat 入口选择同平台、采用 Chat Completions 协议的 API Key 账号；Responses 与协议转换继续开发。网关检查用户/Key/分组权限、IP、到期、客户端模型白名单、余额、多层额度、RPM 和并发，按渠道及账号映射替换模型和上游凭证。

`POST /v1/messages` 支持 Anthropic 原生 JSON/SSE，`/v1/messages/count_tokens` 代理 token 计数。账号需配置 Anthropic 协议；按量兼容平台可使用 `credentials.api_protocol=anthropic`。版本和 beta 协议头受长度限制后转发，签名、工具调用、缓存控制和内容事件保留。缓存命中、5 分钟/1 小时缓存写入分别计量，`message_delta` 采用累计用量，结算成功后才发送 `message_stop`。

Gemini 分组使用 `POST /v1beta/models/{model}:generateContent`、`:streamGenerateContent`（SSE）和 `:countTokens`，模型映射同时应用于 URL 与嵌套 token 计数请求。输出 token 包含思考 token，缓存 token 从普通输入中拆分；原生流保持 Gemini 事件形态，不添加 Chat 的 `[DONE]`。当前原生端点支持文本生成及其多模态输入，媒体生成输出继续开发。模型列表/详情和协议转换尚未实现。

网关支持 Bearer、`X-Api-Key` 和 `X-Goog-Api-Key`；Gemini 原生入口还支持兼容的 `key` 查询参数，该参数不会转发到上游。客户端原始鉴权、Cookie 和任意自定义 header 不透传。两个 token 计数入口仍检查权限、余额与限额，返回计数而不写消费日志或扣余额。

账号选择考虑优先级、并发、额度、到期及冷却；上游 429/502/503/504 在尚未输出时最多尝试三个不同账号。401/403 标记认证错误，429 写入冷却。客户端取消传至上游，结算后释放并发名额。流式请求自动要求 usage，只有完成和结算成功才输出协议对应的结束事件；缺失 usage 返回待核查错误，已收到的 usage 在后续断流时仍结算。

用量、扣费去重、用户余额、Key/账号计数和平台额度同事务写入。日志精度为 10 位，余额与 Key/账号扣款为 8 位；起始余额为正的已发生消费可使余额为负。Redis 保存不含提示词或凭证的待结算记录，后台重试和重启恢复不重复扣费；持久部署须保留 Redis AOF 和 PostgreSQL 数据。上游尚未返回 usage 时的进程崩溃仍需人工核查，不能承诺第三方调用恰好一次。

可选 `Idempotency-Key` 在同一个客户端 Key 下识别重复请求；24 小时内完整 JSON/SSE 响应可重放，同键不同内容返回 409。进程中断遗留的 processing 记录即使过期也拒绝自动重发，需核查原请求；超过 16 MiB 的响应不缓存重放。客户端请求幂等和扣费去重分别记录。

用户通过 `/api/v1/usage`、`/stats`、`/{id}` 和 `/errors` 查询本人原始用量、汇总和错误，管理员通过 `/api/v1/admin/usage`、`/stats` 查询。`GET /v1/billing` 使用客户端 Key 查询余额及额度；余额耗尽仍可查询。管理员通过 `/api/v1/admin/users/{id}/platform-quotas` 的 GET/PUT 配置平台额度，`/reset` 重置指定窗口；用户通过 `/api/v1/user/platform-quotas` 查询。平台额度 NULL 为不限、0 为禁止，日/周按 Asia/Shanghai 自然日/周，月按滚动 30 天。

Key 列表费用查询保留 `POST /api/v1/usage/dashboard/api-keys-usage`（`api_key_ids` 最多 100 个，只返回本人 Key）：`today_actual_cost` 为 Asia/Shanghai 当日费用，原 `total_actual_cost` 字段为近 30 天费用。`GET /api/v1/user/api-keys/{id}/usage/daily` 支持 1–90 天和显式 `timezone`，默认 30 天、Asia/Shanghai。金额直接在数据库精确汇总，不改变原始消费。

管理员审计查询为 `/api/v1/admin/audit-logs` 和 `/{id}`，支持操作者、动作、方法、IP、RFC3339 时间、成功状态及关键词筛选；每页最多 200 条。查询不提供清空能力。`/api/v1/admin/usage/search-users` 与 `/search-api-keys` 为账务筛选提供用户/Key 简要信息，包含历史用户归属，不返回密码和 Key 原文。

## 验证

```sh
go test ./...
go vet ./...
DOCKER_CONTEXT=desktop-linux python3 scripts/check-schema.py
DOCKER_CONTEXT=desktop-linux python3 scripts/test-integration.py
```

集成脚本创建独立的临时 PostgreSQL/Redis，运行 race 检测和真实数据库测试，结束后删除测试容器。普通 `go test` 未配置 `TEST_DATABASE_URL`、`TEST_REDIS_URL` 时跳过数据库集成用例。若给集成脚本提供 `TEST_UPSTREAM_BASE_URL`、`TEST_UPSTREAM_MODEL` 和秘密环境变量 `TEST_UPSTREAM_API_KEY`，会额外发起两次可能计费的真实网关请求，并核对用量、余额及 Key 计数。指定中转的 `gpt-5.6-luna` 已通过该 JSON/SSE 网关验证；测试价格仅用于验证账务，并非上游报价。

数据库定义位于 `schema/baseline.sql`，固定来源及校验和位于 `schema/source.json`。新库省略两张插件表及其专属对象，其余业务表保留原结构和含义。`schema/contract.json` 固定表列、约束、索引、函数、触发器和序列定义；结构改变会拒绝启动。确需变更基线时，审核 SQL 后以 `scripts/check-schema.py --write-contract` 重新生成契约。正常运行不依赖其他项目目录。

`reserve/` 是被 Git 忽略的本地规划目录。来源版权及许可证见 `NOTICE`、`LICENSE`、`COPYING`。
