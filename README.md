# lite-api

面向企业和团队内部使用的 AI 网关，使用 Go、PostgreSQL 和 Redis，单实例部署。管理员创建账户，用户管理自己的 API Key。内置前端目录保留占位，当前开发后端。

目前已实现空库初始化、结构校验、首个管理员初始化、密码登录与令牌刷新/撤销、用户管理、分组基础配置/授权、用户 API Key 管理及管理员余额调整、上游 API Key 账号与代理管理、文本手动测试和定时测试计划、渠道价格配置和可用渠道查询。已接入 Chat Completions、Responses、Anthropic Messages 和 Gemini 原生 JSON/SSE 网关、token 计数、用量与事务扣费、平台额度和基础查询；Responses WS、协议转换、媒体、完整模型广场及扩展运行功能仍在开发中。

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

管理员通过 `GET /api/v1/admin/groups/{id}/rate-multipliers` 查询用户专属倍率和 RPM。`PUT .../rate-multipliers` 接受 `{"entries":[{"user_id":1,"rate_multiplier":0.5}]}`，替换整组倍率，保留 RPM；`PUT .../rpm-overrides` 接受 `{"entries":[{"user_id":1,"rpm_override":10}]}`，替换整组 RPM，保留倍率。未列出的用户恢复该项默认值，RPM 的 `null` 为恢复默认，`0` 为免除该组限制。专属配置不授予分组访问权。`DELETE .../rpm-overrides` 只清除 RPM；沿用既有行为，`DELETE .../rate-multipliers` 清除整组专属记录（包括 RPM），若只清倍率请使用 PUT 空 `entries`。用户编辑中的 `group_rates` 省略时不修改，空对象清除该用户所有专属倍率，值为 `null` 只清该组倍率，均保留 RPM。

用户 `rpm_limit` 是跨 Key、跨分组的全局上限；分组 `rpm_limit` 按用户分别限制，专属 `rpm_override` 只覆盖分组值，不能绕过用户全局上限。`GET /api/v1/admin/users/{id}/rpm-status` 返回当前分钟用户总量、各 Key 所属分组的计数及 group/override 来源；无倍率/RPM 配置也会统计获准请求，拒绝请求不增加计数。修改配置立即生效并保留本分钟计数；Redis 故障返回 503。

## 上游与健康测试

管理员通过 `/api/v1/admin/accounts` 配置 `platform`、`type=apikey`、`credentials.api_key`、可选 `credentials.base_url` 和 `group_ids`。支持的账号平台为 openai、anthropic、gemini、grok、kimi、zhipu、deepseek、minimax；仅接受按量 API Key，账号查询不会返回原始 Key。`POST /api/v1/admin/accounts/{id}/test` 使用 `model_id` 发起真实文本请求，返回测试 SSE；它可能产生上游费用，不计入内部用户消费。

`/api/v1/admin/scheduled-test-plans` 支持创建、编辑、删除，`/{id}/results` 查询结果，`/api/v1/admin/accounts/{id}/scheduled-test-plans` 列出账号计划。这组接口沿用直接 JSON 响应。五字段 cron 默认 UTC，可用 `CRON_TZ=Asia/Shanghai` 指定时区。单实例每 15 秒扫描，按到期顺序执行，每次调用最多 45 秒；`max_results` 控制留存，`auto_recover` 仅恢复可恢复状态，保留人工停调和消费额度。进程停止会取消正在执行的请求，未完成计划下次启动继续检查。

代理支持 HTTP、HTTPS、SOCKS5/SOCKS5h 和到期后的明确备用/直连配置。上游重定向不跟随，TLS 验证不能关闭，公开上游必须使用 HTTPS。内网模型或代理须在部署环境 `UPSTREAM_PRIVATE_CIDRS` 中明确允许目标网段；默认拒绝本机和私网。不要将该配置扩大为任意地址。

独立上游验证无需数据库：设置本地 `UPSTREAM_BASE_URL`、`UPSTREAM_API_KEY`、`UPSTREAM_MODEL` 后执行 `./bin/lite-api upstream-check`。命令发起一次 JSON 和一次 SSE 文本生成，仅输出状态和时延摘要。Go 客户端对指定中转和 `gpt-5.6-luna` 的 JSON/SSE 已实测成功；其他平台当前由协议模拟测试覆盖，不能据此宣称所有原厂模型均已联调。

## 渠道与定价

管理员通过 `/api/v1/admin/channels` 管理渠道、分组关联、模型映射、价格和账号成本规则。一个分组只能属于一个渠道；关联与价格替换在同一事务中完成。价格沿用各字段的十进制精度，token 价格单位为 USD/token，支持科学计数法。`billing_model_source` 可配置 requested、channel_mapped、upstream 或 response_model。Chat Completions 已使用请求开始时的价格快照结算，消费期间修改价格不会回改该次费用。

价格支持 token、per_request、image，缓存读写及 1h 写入价、上下文阶梯、时段/服务等级/推理倍率。上下文阶梯按 `(min_tokens, max_tokens]` 匹配；时段使用显式时区与 `[start_time, end_time)`，结束 `00:00` 表示当天结束。账号成本规则单独保存，不改变用户价格。渠道缺失的文本单价回落到参考价，显式零价仍为免费；单独配置缓存写入价同时覆盖 5m/1h，单独的 1h 价优先。渠道阶梯替代参考阶梯，未配阶梯则继承参考长上下文倍率；渠道自定义价不叠加供应商默认时段策略。`restrict_models=true` 仍要求模型命中渠道价卡，不能通过参考价绕过限制。

内置 [参考价表](internal/app/reference_prices.json) 是截至 2026-09-23 的固定兼容计价基线，包含 248 条价卡及八个平台，不代表上游当前实际售价或全部协议已完成。`GET /api/v1/admin/channels/model-pricing?model=...` 查询参考价（可附 `platform`），`GET /api/v1/admin/channels/pricing/sync-models?platform=...` 枚举当前目录，均返回 `as_of` 和 SHA256；后者不发起联网更新。参考价支持明确的模型 ID、日期后缀和 GPT 推理等级后缀，不按未知名称猜价。中转跨品牌模型可使用唯一匹配的参考价；未知或冲突的身份需配置渠道价。

部署可用 `PRICING_FILE` 指定同格式的完整本地价表；为空时使用内置表。文件上限 8 MiB、10000 条价卡，包含 `as_of`、`source`、`prices`；单价单位 USD/token，模型名必须具体，token 阶梯用倍率表示。目录独有的 `fast_ratio` 支持精确分数（如 `"5/3"`），显式 `fast_multiplier` 优先；展示同时提供精确分数字段。每分钟校验变更后原子发布，建议通过临时文件加 rename 更新。启动时无效文件会拒绝启动；运行中无效或丢失文件保留上一份有效价表并记录错误。单次请求及账号重试共用固定目录快照，更新不改变已开始请求的结算。

`PUT /api/v1/admin/settings` 支持 `site_name`、`available_channels_enabled` 及模型广场开关。可用渠道默认关闭，开启后用户可通过 `/api/v1/channels/available` 查询可访问分组下的具体模型和价格，包含映射别名和本人倍率；私有分组、其他平台模型、内部账号成本规则及映射目标不向无权限用户返回。

`GET /api/v1/model-plaza` 是分组模型价格目录，由 `model_plaza_enabled`（默认关闭）、`model_plaza_require_auth` 和 `model_plaza_description` 控制。匿名只见公开分组；使用登录令牌可查询已授权专属分组和本人的 `user_rate_multiplier`，受公开分组限制的用户仍需授权。无效令牌明确拒绝，API Key 不能代替登录。目录每个 IP 每分钟最多 60 次，响应禁止缓存；不调用上游或产生消费，也不保证列出的模型当前可调度。

广场价格单位为 USD/token，倍率另列。token 阶梯展示绝对单价，与扣费共用解析规则，包括缓存 5m/1h、阶梯空档回落、显式零价、时段/推理倍率；关闭分组长上下文计费后展示基础档。渠道的图片/按次价和档位保留。缺价返回 `null`；按上游/响应模型决定价格时，也不预报未经确认的价格。`official_pricing` 返回可识别模型的固定参考价，目录附 `pricing_as_of` 和 `pricing_checksum`；别名不猜测官方身份。分组媒体专用价格仍待后续实现。

## 网关、用量与账务

客户端用自己创建的 Key 作为 Bearer 凭证调用 `POST /v1/chat/completions`，或兼容入口 `/chat/completions`、`/backend-api/codex/chat/completions`。Chat 入口选择同平台、采用 Chat Completions 协议的 API Key 账号；协议转换继续开发。网关检查用户/Key/分组权限、IP、到期、客户端模型白名单、余额、多层额度、RPM 和并发，按渠道及账号映射替换模型和上游凭证。

`POST /v1/responses` 及 `/responses`、`/backend-api/codex/responses` 支持原生 JSON/SSE，只选择配置 `credentials.api_protocol=responses` 的同平台账号。工具调用、结构化输出与加密推理内容原样传递；支持 function/custom 工具。终止事件在扣费成功后发送，`incomplete` 保留原协议含义；失败响应中的有效 usage 仍结算，思考 token 已包含在输出量中，不重复加算。输入、缓存读、缓存写分别计费，Chat 与 Responses 共用互斥 token 计量。协议字段见 [Responses API](https://developers.openai.com/api/reference/resources/responses/methods/create)。

三个 Responses 前缀均提供 `/compact` 和 `/input_tokens`：压缩按返回 usage 结算，token 计数只验证权限/余额/限额而不扣费，两者不支持流式。原生流式 `compaction_trigger` 会规范为最后一个输入项、补充对应协商头，并保存 `native_compaction_v2` 用量标记。未知子路径拒绝转发。

`previous_response_id` 绑定到原客户端 Key、分组、上游账号及上游凭证/地址，Redis 保存 30 天的关联元数据；缺失、到期或账号停用/轮换后拒绝续接，不切换到另一账号。`store=false` 不建立关联；同幂等键的已完成响应可直接重放。当前只支持通过该网关创建的响应续接。后台 Responses、conversation/item_reference、内置搜索/图片等托管工具及 WS 仍待对应持久任务、隔离和计费实现，当前明确拒绝，未作为已完成能力。

`POST /v1/messages` 支持 Anthropic 原生 JSON/SSE，`/v1/messages/count_tokens` 代理 token 计数。账号需配置 Anthropic 协议；按量兼容平台可使用 `credentials.api_protocol=anthropic`。版本和 beta 协议头受长度限制后转发，签名、工具调用、缓存控制和内容事件保留。缓存命中、5 分钟/1 小时缓存写入分别计量，`message_delta` 采用累计用量，结算成功后才发送 `message_stop`。

Gemini 分组使用 `POST /v1beta/models/{model}:generateContent`、`:streamGenerateContent`（SSE）和 `:countTokens`，模型映射同时应用于 URL 与嵌套 token 计数请求。输出 token 包含思考 token，缓存 token 从普通输入中拆分；原生流保持 Gemini 事件形态，不添加 Chat 的 `[DONE]`。当前原生端点支持文本生成及其多模态输入，媒体生成输出和协议转换继续开发。

`GET /v1/models`、`/models` 及其 `/{model}` 返回当前 Key 分组可见的模型，应用渠道/账号映射和客户端白名单。`/v1beta/models` 及详情返回 Gemini 原生格式，支持 `pageSize`/`pageToken`；上游模型目录按账号及配置版本缓存一分钟，支持上游分页。目录不产生消费，零余额仍可查询，禁用或到期的身份不可查询；ETag 命中也先验证权限。中转没有模型目录接口时可配置账号模型映射，通配映射需有具体目录或白名单模型才能枚举。

`GET /backend-api/codex/models` 或列表请求带 `client_version` 时返回客户端模型 manifest。管理员可在 OpenAI 分组配置 `codex_models_manifest_config`，指定 1–10 个组内账号及 `fallback_to_scheduler`；按配置顺序合并指定目录，缺失能力不虚构上下文大小。`POST /api/v1/admin/accounts/{id}/models/sync-upstream` 同步完整能力到账号元数据，不改模型映射或消费计数；部分元数据返回警告，轮换凭证或地址会清除旧能力快照。复合分组模型发现随复合路由继续开发。

`POST /v1/embeddings` 和 `/embeddings` 支持 OpenAI 分组的向量请求，接受单条/批量文本及 token 序列，保留 `dimensions` 和 `encoding_format=float|base64`。仅选择 Chat/Responses 协议的 API Key 账号；上游模型映射、权限、限额和扣费与文本共用。嵌入请求不支持流式，输入 token 按价卡计费，纯文本嵌入无需配置输出价；缺少有效 usage 不能记成零消费成功。输入形式依据 [OpenAI Embeddings API](https://developers.openai.com/api/reference/resources/embeddings/methods/create)。

网关支持 Bearer、`X-Api-Key` 和 `X-Goog-Api-Key`；Gemini 原生入口还支持兼容的 `key` 查询参数，该参数不会转发到上游。客户端原始鉴权、Cookie 和任意自定义 header 不透传。各 token 计数入口仍检查权限、余额与限额，返回计数而不写消费日志或扣余额。

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

集成脚本创建独立的临时 PostgreSQL/Redis，运行 race 检测和真实数据库测试，结束后删除测试容器。普通 `go test` 未配置 `TEST_DATABASE_URL`、`TEST_REDIS_URL` 时跳过数据库集成用例。若给集成脚本提供 `TEST_UPSTREAM_BASE_URL`、`TEST_UPSTREAM_MODEL` 和秘密环境变量 `TEST_UPSTREAM_API_KEY`，会额外发起两次可能计费的真实网关请求，并核对用量、余额及 Key 计数。指定中转的 `gpt-5.6-luna` 已通过 Chat 和 Responses 的真实 JSON/SSE 网关验证；设置 `TEST_UPSTREAM_PROTOCOL=responses` 可验证 Responses JSON/SSE，默认 `chat_completions`。测试价格仅用于验证账务，并非上游报价。

数据库定义位于 `schema/baseline.sql`，固定来源及校验和位于 `schema/source.json`。新库省略两张插件表及其专属对象，其余业务表保留原结构和含义。`schema/contract.json` 固定表列、约束、索引、函数、触发器和序列定义；结构改变会拒绝启动。确需变更基线时，审核 SQL 后以 `scripts/check-schema.py --write-contract` 重新生成契约。正常运行不依赖其他项目目录。

`reserve/` 是被 Git 忽略的本地规划目录。来源版权及许可证见 `NOTICE`、`LICENSE`、`COPYING`。
