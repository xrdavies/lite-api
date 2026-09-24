# lite-api

面向企业和团队内部使用的 AI 网关，使用 Go、PostgreSQL 和 Redis，单实例部署。管理员创建账户，用户管理自己的 API Key。内置前端目录保留占位，当前开发后端。

目前已实现空库初始化、结构校验、首个管理员初始化、密码登录与令牌刷新/撤销、用户管理、分组基础配置/授权、用户 API Key 管理及管理员余额调整、上游 API Key 账号与代理管理、文本/图片/视频手动测试和定时测试计划、渠道价格配置和模型广场。已接入 Chat Completions、Responses、Anthropic Messages 和 Gemini 原生 JSON/SSE 网关、token 计数、用量与事务扣费、平台额度和基础查询；已支持 Responses WebSocket、Chat/Responses 双向基础转换、alpha 和 Grok 独立搜索，以及 OpenAI/Grok API Key 图片生成/编辑（JSON URL/data URL 与 multipart 文件）和持久异步图片任务、Gemini 原生同步/流式及批量图片任务；已支持 Grok 语音/Realtime、自定义声音和视频生成/编辑/扩展、Seedance 持久任务；托管工具及其余扩展运行功能仍在开发中。

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

`GET /api/v1/admin/users/{id}/usage` 查询指定用户的真实用量汇总，`period=day|week|month`（默认 month）分别从 Asia/Shanghai 当日、周一或当月首日零点统计到查询时刻，返回请求数、包含缓存的 token 总量、用户实际费用和已记录耗时的平均值。`GET /api/v1/admin/accounts/{id}/today-stats` 返回上游账号当日的 requests、tokens、cost、standard_cost、user_cost；账号成本使用历史 `COALESCE(account_stats_cost,total_cost) × COALESCE(account_rate_multiplier,1)`，标准费用使用 total_cost，用户费用使用 actual_cost。金额由数据库精确汇总，当前倍率修改不改变历史结果；已删除 Key 的消费继续计入，不存在或已删除的用户/账号返回 404。两个接口仅管理员可用，不查询上游或调用复杂统计任务。

复合分组使用 `platform=composite`，可关联八种已支持平台的 API Key 账号。管理员通过 `/api/v1/admin/groups/{id}/composite-routes` 的 GET/POST 和 `/{route_id}` 的 PUT/DELETE 管理路由，POST 成功返回 201，PUT 为整条替换；`/preview` 接受 `model`、`endpoint`，只预览配置决策，不代表上游当前可用。路由包含 `public_model`、`match_type=exact|prefix`、`target_platform`、`upstream_model`、`endpoint`、`priority`、`enabled`、`notes`；endpoint 支持 any/messages/count_tokens/responses/chat_completions/embeddings/images/gemini；images 可调度 OpenAI 或 Grok API Key 图片生成/编辑，Grok 编辑请求会转换为其原生 JSON 图片对象。

复合路由按精确匹配、指定端点、最长前缀、priority 升序、ID 升序选择。空 `upstream_model` 在 exact 时采用 public_model，prefix 时透传具体请求模型。无显式命中时，先使用账号精确模型映射确定归属，多平台争用同一别名则拒绝；再识别已知厂商模型前缀，未知名称拒绝。路由选择平台后，依次应用渠道和账号模型映射。客户端白名单在改写前校验，requested 计价和日志保留公共模型名；平台额度按解析出的具体平台检查、结算。不会跨平台重试，Responses 续接仍绑定原账号和上游来源。当前覆盖已有原生文本/Responses、计数及 Embedding 路径；协议转换与媒体继续开发。

分组可配置 `claude_code_only` 和 `fallback_group_id`。Messages 使用 CLI User-Agent、必要请求头、system 特征和 metadata 识别客户端，兼容单 token 探测及 count_tokens 辅助请求。这是客户端分类规则，所有请求仍必须通过 API Key 鉴权。非匹配客户端在配置 fallback 时使用目标组账号，未配置返回 403；Chat、Responses、Embedding 入口在开启限制时直接拒绝。模型发现和用量查询仍属于原 Key 分组。

fallback 仅委托调度：原 Key 归属、模型白名单、用户价格/倍率、RPM 和用量 group_id 保持原分组；目标组账号必须满足平台、协议、健康、并发、额度和目标渠道模型限制。管理员可指定私有目标组，但用户不会因此获得在目标组建 Key 的权限。平台额度继续按原具体平台计量，原分组为 composite 时按解析平台计量。目标组停用/删除立即停止新派发；排队期间关系或策略变化会重新检查并拒绝旧请求，在途消费按开始时的价格结算。fallback ID 省略/null 保持，0 或负数清除；配置拒绝自引用、循环、受限目标及范围外资源。上游 HTTP 400/503 不触发跨分组 fallback。

分组可配置 `model_routing_enabled` 和 `model_routing`，例如 `{"gpt-*": [12, 18]}`。规则匹配渠道改写后的模型名，区分大小写，精确项优先，其次最长尾部 `*` 前缀；空列表不形成优先池。规则用于 OpenAI、Anthropic 目标平台，复合分组按解析平台执行。优先池内仍按账号优先级和最后使用时间选择，数组顺序不代表优先级；不可用时回退同组其他合格账号，所有权限、协议、额度、并发和渠道模型限制继续生效。规则中的未关联、已删除或其他平台账号不会被调用。省略或 `null` 保留配置，`{}` 清空；开关关闭时保留规则。Responses 续接优先遵守原账号绑定，原账号不可用时拒绝转投。

未固定模型目录账号时，manifest 的能力取模型优先池内账号的交集；优先账号无可用元数据时不借用普通候选的能力。复合分组同名模型在其他端点平台可调用时，仍以共同能力为限。显式 `codex_models_manifest_config` 保持所选目录账号的顺序。目录展示不承诺此刻有并发空位，也不保证回退账号拥有相同上下文上限。

OpenAI、Anthropic 和复合分组可配置 `max_reasoning_effort`、`max_reasoning_effort_over_limit= downgrade|deny` 和 `reasoning_effort_mappings`。上限支持 minimal/low/medium/high/xhigh/max，Anthropic 不接受 minimal；复合分组派发到 Anthropic 时将 minimal 策略目标适配为 low。映射格式为 `{"from":"max","to":"high","match_type":"prefix","model":"gpt-"}`，最多 64 条；from 另可为 none，to 另可为 deny。匹配客户端原始模型名，精确优先于最长前缀/后缀，再到全局；同级按数组顺序，仅执行一次映射，随后执行上限。超限 deny 或映射 deny 返回 403，不调用上游。

策略处理显式 `reasoning.effort`、`reasoning_effort`、`output_config.effort`，保留嵌套其他字段；缺省值不补写，未知值交由上游处理。Chat、Responses、Messages 及其复合派发均使用实际转发 effort 的价格倍率；用量中的 `requested_reasoning_effort` 单独保留规范化的客户端请求值（未知/none 为 null，缺省时可记录模型名的已知后缀）。在途修改策略不改变该次结算，已完成的幂等请求仍可原样重放。省略/null 保留配置，空上限取消限制，空映射数组清除规则；策略不用于 Gemini 或其他具体平台。

管理员还可通过 `GET /api/v1/admin/accounts/{id}/usage` 查询适用的账号额度信息。Grok 返回已观察到的请求/token 限额、重置时间、Retry-After 与时间戳，并附本地当日及滚动 24 小时精确用量；HTTP（含 SSE 响应头）与 WebSocket 握手共用采集，尚无观测时明确返回 `quota_unknown`。快照仅保留已解析数值，不保存任意头、Cookie 或 plan 声明；不改变账号配置版本、消费计数或人工状态。凭证、地址、协议及代理变化清除快照，旧请求不能覆盖新配置。无额度头的普通成功响应保留上次观测及原时间戳；401/403/429 记录最近拒绝，不把缺少额度当成零。快照是历史观测，不保证当前剩余额度，也不直接改变调度。

Gemini 同一路径返回 `source=local`、`quota_basis=compatibility_default` 的本地估算，按模型名 flash/lite 与其余 Pro 分类；每日按 America/Los_Angeles 自然日（含夏令时），每分钟按固定分钟统计。`credentials.tier_id` 仅 Gemini 接受空字符串、`aistudio_free`、`aistudio_paid`；缺省沿用固定兼容值 Pro 50/日、2/分钟及 Flash 1500/日、15/分钟，paid 不显示日额度、分钟分别为 1000/2000。这些值不是实时原厂额度，估算不用于调度；历史 `gemini_quota_policy` 自定义规则尚未接入。窗口 `cost` 沿用用户实际消费口径。

该查询接受 `source=active|passive` 和 `force=true|false`，在当前 API Key 范围内均只读，不发起收费探测或自动恢复账号；其他六个平台明确返回 400，原始本地用量仍通过 `today-stats` 查询。`POST /api/v1/admin/accounts/check-mixed-channel` 接受 `platform`、`group_ids` 和可选 `account_id`，对允许的平台返回 `has_risk=false`；原告警所依赖的平台组合已不在范围内，实际绑定仍由账号保存接口校验。

## 上游与健康测试

管理员通过 `/api/v1/admin/accounts` 配置 `platform`、`type=apikey`、`credentials.api_key`、可选 `credentials.base_url` 和 `group_ids`。支持的账号平台为 openai、anthropic、gemini、grok、kimi、zhipu、deepseek、minimax；仅接受按量 API Key，账号查询不会返回原始 Key。`POST /api/v1/admin/accounts/{id}/test` 使用 `model_id` 发起真实请求，返回测试 SSE；它可能产生上游费用，不计入内部用户消费。`mode` 支持 default（或省略）、text、image、video，以及 Grok 专用 search、tts、stt、realtime；自动模式按账号映射后的已知模型识别 OpenAI/Grok/Gemini 图片、Grok 视频和 Seedance，其余走文本。显式 text 强制文本协议，自定义媒体模型名可显式选择 image/video；Seedance 始终要求账号能力开启。

图片测试返回 `image` 事件，视频创建后轮询至成功并返回 `video` 事件，只有真实结果通过校验才发送 `test_complete`。`image_data_url` 接受最多 8 MiB 的 PNG/JPEG/WebP/GIF base64 图片，用于图片编辑或视频首帧；输入 JSON 上限 12 MiB，媒体响应上限 16 MiB。原厂 OpenAI 编辑使用 [JSON 图片引用格式](https://developers.openai.com/api/reference/resources/images/methods/edit)，Grok 使用其 image 对象，Gemini 使用 inlineData。文本上限 45 秒、媒体上限 90 秒；视频超时按失败记录，并保留已接收任务 ID 供管理员核查，不重发创建。测试结果中的图片和签名 URL 只向管理员响应，不存入定时结果表。

Grok `search` 沿用独立搜索的 `grok-4.6` 和账号映射，向 `/v1/responses` 请求 web_search，必须有工具调用或有效来源证据才成功；prompt 是查询词。`tts` 使用 prompt 作为文本，返回最多 4 MiB 的 `audio` SSE；不会截断音频或重试可能已计费的请求。`stt` 接受最多 8 MiB 的 `audio_data_url`，缺省发送 0.25 秒静音 WAV，只验证接口可用性；空转写可以成功，无效返回不算成功。TTS/STT 不使用文本模型映射，STT 使用上游默认模型，选项先于文件发送。请求协议参考 [xAI TTS](https://docs.x.ai/developers/model-capabilities/audio/text-to-speech)、[STT](https://docs.x.ai/developers/model-capabilities/audio/speech-to-text) 和 [搜索](https://docs.x.ai/developers/tools/web-search)。

`realtime` 使用所选模型及账号映射，缺省 grok-voice-latest。握手上限 12 秒，随后最多 3 秒观察首个事件；错误事件、畸形帧、提前断开均失败，无事件则明确报告仅握手成功。不会发送音频或生成指令，不表示语音质量或持续会话验证通过；服务端事件正文不会进入诊断结果。参见 [xAI Voice Agent](https://docs.x.ai/developers/model-capabilities/audio/voice-agent)。这四个模式仅供管理员手动测试；原定时计划没有 mode 字段，仍按文本/图片/视频自动选择。

手动和定时测试共用账号并发上限，同一账号最多执行一个健康测试。测试可以探测停调账号，成功恢复仍检查配置版本，不覆盖人工禁用、停调开关和本地额度；失败不冒充健康。测试只证明所测模型的本次调用结果。

`/api/v1/admin/scheduled-test-plans` 支持创建、编辑、删除，`/{id}/results` 查询结果，`/api/v1/admin/accounts/{id}/scheduled-test-plans` 列出账号计划。这组接口沿用直接 JSON 响应。五字段 cron 默认 UTC，可用 `CRON_TZ=Asia/Shanghai` 指定时区。单实例每 15 秒扫描，按到期顺序执行，按上述文本/媒体超时执行；`max_results` 控制留存，`auto_recover` 仅恢复可恢复状态，保留人工停调和消费额度。计划不新增模式列，按映射后的模型自动选择文本/图片/视频，结果只保存状态、耗时和简短摘要。进程停止会取消正在执行的请求，未完成计划下次启动继续检查。

代理支持 HTTP、HTTPS、SOCKS5/SOCKS5h 和到期后的明确备用/直连配置。上游重定向不跟随，TLS 验证不能关闭，公开上游必须使用 HTTPS。内网模型或代理须在部署环境 `UPSTREAM_PRIVATE_CIDRS` 中明确允许目标网段；默认拒绝本机和私网。不要将该配置扩大为任意地址。

代理到期回退同时用于 HTTP 和 WebSocket。运行时立即沿显式备用链选择有效代理或 direct；无可用路径、循环、链超过九个节点时拒绝派发，人工停用的起始代理不会启用回退。单实例启动时及每分钟扫描最多 100 个到期 active 代理，在同一事务中标记 `expired`、移动账号的 `proxy_id` 并将首次原代理保存在 `proxy_fallback_origin_id`；多次回退保留首次来源，无解时保留原绑定并阻止连接。事务失败整体回滚，下次扫描继续。

管理员先续期并激活原代理，再调用 `POST /api/v1/admin/accounts/{id}/revert-proxy-fallback` 还原，成功返回 `{"message":"reverted"}`；续期本身不移动账号。未处于回退状态或原代理不可用返回 409，账号不存在返回 404。手工设置账号 `proxy_id`（包括 0 直连）清除原代理记录；仍被账号当前绑定、原代理记录或备用链引用的代理不能删除。代理编辑、到期处理、账号指派和还原共用事务锁，避免并发续期被旧结果覆盖。代理及其备用链变更会清除受影响账号的倍率/模型快照并更新版本，消费累计和其他 JSON 保持；已通过路由选择的在途请求可以完成，新请求使用当前配置。

`POST /api/v1/admin/proxies/{id}/quality-check` 检查指定代理的基础出口连通及 OpenAI、Anthropic、Gemini、Grok 四个固定目标，不发送上游 API Key、管理员凭证或 Cookie，也不允许请求指定检测 URL。原 `/test` 使用相同基础检查。两者都直接检测指定代理，即使它已停用/到期也不使用备用代理或直连；它们不会改变代理/账号的调度或到期状态。基础检查须获得合法出口 IP；失败时停止后续目标检测。各目标允许的未鉴权状态代表可达，不证明模型调用权限；429 记 warn，挑战页记 challenge，其他非预期状态记 fail。挑战识别参考 [Cloudflare cf-mitigated 说明](https://developers.cloudflare.com/cloudflare-challenges/challenge-types/challenge-pages/detect-response/)，并保留 403/429 HTML 特征检测，不尝试绕过挑战。

质量响应包含 items、各类计数、score、grade、summary 和 checked_at；分数为 `max(0,100−10×warn−22×fail−30×challenge)`，90/75/60/40 分分别为 A/B/C/D，其余 F。基础连通失败可能仍有数值 B，实际可用性以逐项状态和 `quality_status` 为准。每目标最多 15 秒、总计 80 秒，响应头最多 64 KiB，正文只读取 8 KiB 分类前缀（基础 trace 必须完整且不超限）；不重试、不跟随重定向，继续校验 DNS、私网范围和 TLS 证书。单代理只允许一个检查，全局最多四个并发。诊断只保留固定摘要、出口 IP/国家代码/机房和格式有效的 cf-ray，不保存目标正文或代理密码。

检查结果在 Redis 保留 24 小时，按代理数据库版本隔离；代理详情及列表显示当前版本的 latency/quality 字段，读取不触发探测。基础测试与质量检查分别保存，基础测试不抹去完整质量结果，连通字段以最新完成结果为准。检查期间代理配置发生变更返回 409，旧版本快照不会在新配置下显示；Redis 失败仍返回实际检测结果和 `cached=false`，管理查询仍可读取数据库信息。诊断缓存不作为调度或计费依据。

独立上游验证无需数据库：设置本地 `UPSTREAM_BASE_URL`、`UPSTREAM_API_KEY`、`UPSTREAM_MODEL` 后执行 `./bin/lite-api upstream-check`。命令发起一次 JSON 和一次 SSE 文本生成，仅输出状态和时延摘要。Go 客户端对指定中转和 `gpt-5.6-luna` 的 JSON/SSE 已实测成功；其他平台当前由协议模拟测试覆盖，不能据此宣称所有原厂模型均已联调。

管理员可通过 `/api/v1/admin/accounts/upstream-billing-probe/settings` 的 GET/PUT 配置倍率探测，默认 `{"enabled":true,"interval_minutes":30}`，间隔接受 5–1440 分钟。账号默认不参与；PUT `/api/v1/admin/accounts/{id}/upstream-billing-probe` 接受 `{"enabled":true}`，POST 同路径立即探测，POST `/api/v1/admin/accounts/upstream-billing-probe/batch` 接受最多 20 个 `account_ids`，去重并分别返回结果。全局开关只控制定时执行，手动探测不受它限制。单实例每分钟扫描到期账号，每轮最多 20 个、最多四个并发，同账号禁止重叠。

探测支持八种已保留平台的中转账号，统一使用 Bearer Key 请求独立协议 `GET /v1/lite-api/billing`，版本根和自定义路径沿用账号配置；国内平台的 `/anthropic` 或 `/anthropic/v1` 后缀先还原。原厂域名直接记录 unsupported；此能力不是通用供应商余额接口，不支持本声明协议的中转不会产生有效倍率。客户端也可使用自己的 Key 调用此接口，返回 `object=lite-api.key_billing`、`schema_version=1`、`billing_scope=token`、组倍率、适用的用户专属倍率及生效倍率，零余额仍可查询。标准分组不应用订阅峰时系数，模型/媒体价格另行配置。

`GET /api/v1/admin/accounts/upstream-billing-rates` 按账号列表相同的 search/platform/status 和分页条件读取持久快照，支持 ETag/304，不触发探测。快照使用原 `accounts.extra.upstream_billing_probe` 字段，保存白名单声明和时间、状态，失败保留上次有效数据及原 freshness；404/405 或原厂标记 unsupported，重探间隔扩大八倍并封顶一天，正常间隔有抖动，所有重探均尊重更长的 Retry-After。连接及正文共限 10 秒，响应最多 64 KiB；不跟随重定向、不重试未知结果，错误摘要不保存上游正文或凭证。

账号编辑 `upstream_billing_rate_sync_enabled=true` 同时开启探测；创建时需先建账号再开启同步。同步只把声明的 `resolved_rate_multiplier` 四舍五入到四位小数写入账号成本倍率，自动值必须大于 0 且不超过 100；声明中的峰时系数不写入。声明校验包含精确数值、倍率关系及峰时时区/区间，未知字段丢弃。有效但超出自动同步范围的声明仍可查看，账号倍率保持原值。启用同步时手工改倍率返回 409，可在同次编辑关闭同步后修改；关闭探测也关闭同步。账号或代理及其备用链在探测期间变更时拒绝写回；更换账号凭证、来源或代理绑定清除旧快照。探测不恢复健康状态、不改写用户余额、消费额度或历史费用；后续请求沿用现有账号成本结算。

## 运行设置

管理 API 支持密码登录产生的 Bearer JWT，以及独立的全局管理员机器凭证。`GET /api/v1/admin/settings/admin-api-key` 返回 `exists/masked_key`；`POST .../admin-api-key/regenerate` 生成 `admin-` 前缀的 32 字节随机 Key，仅本次响应返回完整值；`DELETE .../admin-api-key` 删除凭证。初次生成使用管理员 JWT，此后合法机器凭证也可轮换或删除自身。原 `settings.admin_api_key` 保持原始字符串语义，应与上游凭证一样限制数据库访问；通用设置、状态查询和审计均不返回完整 Key。

机器调用仅在 `/api/v1/admin/...` 管理接口使用 `X-Api-Key`，每次请求读取当前凭证并以 ID 最小的可用管理员记录操作，审计 `auth_method=admin_api_key`；禁用、删除或降权的管理员不会成为操作身份。凭证是全局设置，不属于生成它的个人账户；密码/会话撤销不代替机器 Key 轮换。与 Authorization 同时提供时，X-Api-Key 优先，错误或重复值不会退回 JWT。它不创建登录会话，不能用来调用本人 Key 管理或模型网关，也不会增加管理员代创建、通用编辑或单独删除用户 Key 的能力。轮换/删除提交后，新请求立即失效旧 Key，已经通过鉴权的在途操作可完成。管理限流沿用所解析管理员的策略，余额调整继续使用原幂等和账务事务。

管理员可对 `/api/v1/admin/settings/overload-cooldown`、`/rate-limit-429-cooldown` 和 `/panel-rate-limit`（均使用相同 settings 前缀）执行 GET/PUT。配置保存在原 settings 表，写入有权限校验和审计，不向公开设置返回。PUT 替换整组配置，客户端应提交完整对象。

529 设置为 `{"enabled":true,"cooldown_minutes":10}`，范围 1–120 分钟；收到 529 后暂停该账号调度，并在未输出响应时沿用最多三个不同账号的换号规则。429 设置为 `{"enabled":true,"cooldown_seconds":5}`，范围 1–7200 秒，仅在缺少有效 `Retry-After` 时使用；关闭默认冷却仍尊重有效的上游秒数或 HTTP 日期（最多两小时）。禁用时提交越界时长会归一化为默认值。更新配置不清除已生效的冷却；如需立即恢复，使用原账号恢复接口。502/503/504 保留独立的短暂冷却，账号认证失败和已识别的余额不足继续按各自规则处理。

管理 API 限流默认为 `{"enabled":true,"user_rpm":240,"heavy_rpm":60,"exempt_admin":true,"public_ip_rpm":300}`。RPM 接受 0–100000，0 不限制该档；认证接口按用户计数，本人 usage 和 Key 每日用量同时计入重查询档。公开设置与模型广场共用 IP 桶，私网/回环来源跳过该档；只使用连接来源地址，不信任客户端转发头。每个桶从首个请求起计 60 秒，超限返回 429 和 `Retry-After`。写入后下一请求生效，普通配置读取缓存 60 秒；读配置失败保留最近有效值，限流 Redis 错误放行，身份认证和登录保护仍独立执行。管理 API 限流与客户端模型调用 RPM 互不混用。

`GET/PUT /api/v1/admin/settings/beta-policy` 管理 Anthropic beta 请求头策略，保存于 `settings.beta_policy_settings`。`rules` 每项包含 `beta_token`、`action`（pass/filter/block）、`scope`（all/apikey）、可选 `error_message`、`model_whitelist`、`fallback_action` 和 `fallback_error_message`。模型按最终上游 ID 匹配，区分大小写，支持末尾 `*`；白名单外未指定 fallback 时透传。所有 block 检查原请求 token，不被先前 filter 抵消；重复头合并，输出去重。默认过滤 fast-mode beta，并仅对固定 Sonnet 5 名称族保留 context-1m beta；这是兼容配置，管理员应按实际供应商能力调整。PUT 替换规则，`rules:[]` 清空；最多 100 条规则，每条最多 100 个模型模式。OAuth/Bedrock scope 不接受。配置每次请求读取，数据库不可用时拒绝带 beta 的派发；缺失或畸形 JSON 保持兼容默认，合法 JSON 内的非法规则报错。仅处理真实 Anthropic Messages/count_tokens 上游；转换到其他协议时原 beta 不透传。filter 只删除请求头 token，涉及的请求体能力仍由上游校验。

`GET/PUT /api/v1/admin/settings/rectifier` 保存原 `rectifier_settings` 五个字段。默认 `enabled`、`thinking_signature_enabled`、`thinking_budget_enabled` 为 true，`apikey_signature_enabled` 为 false，`apikey_signature_patterns` 为 []。本项目只有 API Key 账号，签名纠正使用总开关和独立的 API Key 开关；`thinking_signature_enabled` 保留字段含义，当前无其他认证分支使用它。自定义错误关键词忽略大小写，最多 50 个，每个 500 字节，空白项移除。PUT 替换完整配置，管理权限与写入审计沿用通用入口。

纠正只在 Messages 上游返回明确 HTTP 400 后发生，同一账号最多额外请求一次，且请求体必须有实际变化；不重试连接异常、count_tokens、已成功的 JSON/SSE 或第二次拒绝。签名纠正仅限映射后的 claude-/opus-/sonnet-/haiku- 模型：关闭顶层 thinking、将可见 thinking 历史转为文本、移除 redacted thinking/空文本、为空内容补占位，并移除依赖 thinking 的 context edit；工具调用和结果保留。需要原样回传 thinking 的第三方和未知模型不做签名纠正。预算纠正只匹配明确的 1024 最小预算或 Baseten final-answer reserve 错误，设 budget_tokens=32000、max_tokens 不足 32001 时设为 64000，adaptive 跳过；可能增加最终请求的生成上限。配置不可读时不纠正。错误正文最多读取 64 KiB，不输出或记录供应商原始错误；成功响应仍走原用量/幂等/事务结算。原生、Chat/Responses 转 Anthropic 及账号健康测试复用同一入口，健康测试不计用户消费。当前不做二次工具块转文本重试；相关拒绝仍返回错误。参考 [Anthropic thinking 文档](https://platform.claude.com/docs/en/build-with-claude/extended-thinking)，上述固定纠正值属于网关兼容策略。

`GET/PUT /api/v1/admin/settings/stream-timeout` 配置流停顿后的账号处置，默认 `{"enabled":false,"action":"temp_unsched","temp_unsched_minutes":5,"threshold_count":3,"threshold_window_minutes":10}`。`action` 接受 `temp_unsched`、`error`、`none`，时间范围 1–60 分钟、阈值 1–10 次；PUT 提交完整对象。启用后，单账号在从首次超时起的固定窗口内累计到阈值时临时停调或标记错误；`none` 和禁用不累计。策略每次超时从数据库读取。管理员改账号、恢复或轮换凭证后重新累计，旧请求不能覆盖新状态；临时停调不会缩短已有更长的窗口，也不重置消费数据。配置读取失败不修改账号；Redis 不可用时按一次已确认超时判断，不虚构多次失败。

超时检测独立于上述处置开关：`GATEWAY_STREAM_DATA_INTERVAL_TIMEOUT` 默认 180 秒（允许 30–300），`GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT` 默认 900 秒（允许 60–1800），0 关闭对应间隔检测。适用于 Chat、Messages、Responses、Gemini SSE 及适用转换，Responses WebSocket 每轮采用文本间隔；Gemini 原生/转换图片流采用图片间隔。等待响应头仍受 30 秒传输限制；拿到成功响应头后，读取上游字节的等待超过间隔才算流超时。写入慢客户端的时间、客户端取消、正常 EOF、协议错误和请求总时限不计为账号流超时。SSE 与 Responses WS 单轮上限 30 分钟，其他同步请求仍为 5 分钟。超时返回 504，已开始的流发送错误事件，不发成功终止事件；已观察到的用量仍结算，不自动重发已接受的请求。Grok 双向语音会话继续使用自己的空闲策略。

## 渠道与定价

`GET /api/v1/admin/groups/{id}/model-allowlist-candidates` 为管理员配置模型白名单提供 `models` 候选数组。`id=0` 用于建组前查询；可选 `platform` 覆盖平台，省略时取分组平台，无分组时默认 anthropic。候选来自当前固定参考价目录中的具体模型，以及本组可调度 API Key 账号的请求侧映射名（含通配符）；composite 合并保留平台，结果排序去重。不受当前白名单裁剪、不返回映射目标或凭证，也不会调用上游。候选不是实际可用性声明；真实目录与能力应通过模型发现/健康测试确认。

管理员通过 `/api/v1/admin/channels` 管理渠道、分组关联、模型映射、价格和账号成本规则。一个分组只能属于一个渠道；关联与价格替换在同一事务中完成。价格沿用各字段的十进制精度，token 价格单位为 USD/token，支持科学计数法。`billing_model_source` 可配置 requested、channel_mapped、upstream 或 response_model。Chat Completions 已使用请求开始时的价格快照结算，消费期间修改价格不会回改该次费用。

价格支持 token、per_request、image，缓存读写及 1h 写入价、上下文阶梯、时段/服务等级/推理倍率。上下文阶梯按 `(min_tokens, max_tokens]` 匹配；时段使用显式时区与 `[start_time, end_time)`，结束 `00:00` 表示当天结束。账号成本规则单独保存，不改变用户价格。渠道缺失的文本单价回落到参考价，显式零价仍为免费；单独配置缓存写入价同时覆盖 5m/1h，单独的 1h 价优先。渠道阶梯替代参考阶梯，未配阶梯则继承参考长上下文倍率；渠道自定义价不叠加供应商默认时段策略。`restrict_models=true` 仍要求模型命中渠道价卡，不能通过参考价绕过限制。

管理员创建或更新分组时可设置 `model_pricing`，格式与渠道价卡相同；省略或 `null` 保留现值，`[]` 清空。分组价按选定的计费模型名匹配，精确名优先于首个通配项；价卡平台标签不限制分组内的名称匹配。匹配后整张替代渠道卡，缺省单价继承参考价，显式零价生效；不能绕过渠道模型限制。分组 token 卡只覆盖基础价，所存自定义区间不参与 token 计费，长上下文使用参考阶梯并受分组开关控制；按次/图片卡保留自身档位。分组价不支持时段配置；订阅专属的高峰分组倍率不在按量业务中启用。网关请求固定分组价快照，模型广场使用相同解析规则；可用渠道查询展示渠道价，分组实际价格以模型广场为准。

内置 [参考价表](internal/app/reference_prices.json) 是截至 2026-09-23 的固定兼容计价基线，包含 248 条价卡及八个平台，不代表上游当前实际售价或全部协议已完成。`GET /api/v1/admin/channels/model-pricing?model=...` 查询参考价（可附 `platform`），`GET /api/v1/admin/channels/pricing/sync-models?platform=...` 枚举当前目录，均返回 `as_of` 和 SHA256；后者不发起联网更新。参考价支持明确的模型 ID、日期后缀和 GPT 推理等级后缀，不按未知名称猜价。中转跨品牌模型可使用唯一匹配的参考价；未知或冲突的身份需配置渠道价。

部署可用 `PRICING_FILE` 指定同格式的完整本地价表；为空时使用内置表。文件上限 8 MiB、10000 条价卡，包含 `as_of`、`source`、`prices`；单价单位 USD/token，模型名必须具体，token 阶梯用倍率表示。目录独有的 `fast_ratio` 支持精确分数（如 `"5/3"`），显式 `fast_multiplier` 优先；展示同时提供精确分数字段。每分钟校验变更后原子发布，建议通过临时文件加 rename 更新。启动时无效文件会拒绝启动；运行中无效或丢失文件保留上一份有效价表并记录错误。单次请求及账号重试共用固定目录快照，更新不改变已开始请求的结算。

`PUT /api/v1/admin/settings` 支持 `site_name`、`available_channels_enabled` 及模型广场开关。可用渠道默认关闭，开启后用户可通过 `/api/v1/channels/available` 查询可访问分组下的具体模型和价格，包含映射别名和本人倍率；私有分组、其他平台模型、内部账号成本规则及映射目标不向无权限用户返回。

`GET /api/v1/model-plaza` 是分组模型价格目录，由 `model_plaza_enabled`（默认关闭）、`model_plaza_require_auth` 和 `model_plaza_description` 控制。匿名只见公开分组；使用登录令牌可查询已授权专属分组和本人的 `user_rate_multiplier`，受公开分组限制的用户仍需授权。无效令牌明确拒绝，API Key 不能代替登录。目录每个 IP 每分钟最多 60 次，响应禁止缓存；不调用上游或产生消费，也不保证列出的模型当前可调度。

广场价格单位为 USD/token，倍率另列。token 阶梯展示绝对单价，与扣费共用解析规则，包括缓存 5m/1h、阶梯空档回落、显式零价、时段/推理倍率；关闭分组长上下文计费后展示基础档。渠道的图片/按次价和档位保留。缺价返回 `null`；按上游/响应模型决定价格时，也不预报未经确认的价格。`official_pricing` 返回可识别模型的固定参考价，目录附 `pricing_as_of` 和 `pricing_checksum`；别名不猜测官方身份。Gemini 模型另返回按 1K/2K/4K 分档的 `image_pricing`，与原生图片结算共用价格解析；明确的 token 价卡继续以 token 单价展示。

## 网关、用量与账务

客户端用自己创建的 Key 作为 Bearer 凭证调用 `POST /v1/chat/completions`，或兼容入口 `/chat/completions`、`/backend-api/codex/chat/completions`。Chat 入口选择同平台的 API Key 账号；Chat Completions 协议直接转发，OpenAI/Kimi/Zhipu/DeepSeek/MiniMax 的 Responses 协议账号可通过转换承接。网关检查用户/Key/分组权限、IP、到期、客户端模型白名单、余额、多层额度、RPM 和并发，按渠道及账号映射替换模型和上游凭证。

Chat → Responses 转换支持 JSON/SSE、system/developer/user/assistant 消息、图片/文件输入、函数和自定义工具及结果、旧 functions/function_call、推理摘要、拒绝、结构化输出和服务等级。函数定义展开并默认显式 `strict=false`，`response_format` 映射为 `text.format`，`max_completion_tokens` 优先于 `max_tokens`，上游固定 `store=false`。字段映射依据 [OpenAI 迁移文档](https://developers.openai.com/api/docs/guides/migrate-to-responses)。没有等价映射的非默认控制（如多候选、stop、seed、logprobs）在派发前拒绝；原生 Chat 路径仍按原协议处理。托管工具计费、音频输出和其他协议转换继续开发。

转换输出保留客户端模型名，实际上游模型和 `/v1/responses` 单独写入用量；计价采用原分组/渠道快照与 Responses 实际 usage。SSE 保留增量文本、推理及工具参数，补齐只有终态出现的内容并避免重复；`incomplete` 映射为对应的 length/content_filter，失败或断流不伪造成功。只有结算成功才发送 finish_reason 和 `[DONE]`；usage 块由客户端 `stream_options.include_usage` 决定。三种 Chat 路径继续共享幂等重放，转换不会建立原生 Responses 的续接关联。此转换已通过本地协议服务和数据库验证，尚未新增真实上游转换联调。

Chat 入口也支持 Anthropic 平台，以及 OpenAI/Kimi/Zhipu/DeepSeek/MiniMax 的 `api_protocol=anthropic` 账号。请求转为 `/v1/messages`，支持系统指令、文本/图片/PDF、函数和自定义工具及结果、停止序列和结构化输出；输出转回 Chat JSON/SSE。缓存读写按 Anthropic 原始 usage 分别结算，Chat usage 的 prompt_tokens 包含缓存部分；流式完成标记仍等待扣费成功。复合路由和模型目录采用相同准入。

转换默认 max_tokens=8192，显式 max_completion_tokens 优先。xhigh 转为 max 并按实际 effort 计价；thinking budget 限制在输出上限以内，上限不够时拒绝。缓存标记传到 Anthropic；不透明思考签名不作为 Chat 文本输出。跨厂商 file_id、非默认多候选及没有对应含义的控制在派发前拒绝。协议依据 [Anthropic 流式文档](https://platform.claude.com/docs/en/build-with-claude/streaming) 与 [结构化输出文档](https://platform.claude.com/docs/en/build-with-claude/structured-outputs)，已做本地协议及数据库验证，尚未使用真实 Anthropic 凭证联调。

Chat 三个入口也支持 Gemini 账号，通过原生 `generateContent` / `streamGenerateContent` 返回 Chat JSON/SSE，复合路由和模型发现同步支持。转换保留系统指令、文本、图片/PDF、函数及 custom 工具、工具结果、停止序列、JSON 输出；schema 使用原生 JSON Schema 字段，避免删减约束。客户端函数签名通过 `tool_calls[].extra_content.google.thought_signature` 回传，缺少签名的历史调用使用兼容标记；网关不拉取客户端媒体 URL，URL 可访问性由上游决定。原生字段见 [Gemini generateContent](https://ai.google.dev/api/generate-content)。

Gemini 2.5 的显式 effort 转为 thinkingBudget，3 系列转 thinkingLevel；xhigh/max 映射 high，minimal 在 Pro 上映射 low，未指定时沿用模型默认。计费档位仅取实际 thinkingLevel，预算值不推断收费档位；客户端显式 effort 单独记入请求字段。禁止关闭强制思考，不能映射的多候选、托管工具或禁用并行函数调用等控制在派发前拒绝；预算映射参考 [Gemini OpenAI 兼容文档](https://ai.google.dev/gemini-api/docs/openai)。Chat 输入用量含缓存读，输出用量含思考；扣费使用原生互斥计量。文本/思考增量实时返回，工具调用、finish_reason、usage 和 DONE 等待结算成功。缺用量或断流返回错误，已知消费仍持久结算并支持恢复。本地协议服务及 Docker 数据库验证已通过，尚未使用真实 Gemini Key 联调。

`POST /v1/responses` 及 `/responses`、`/backend-api/codex/responses` 支持原生 JSON/SSE，使用配置 `credentials.api_protocol=responses` 的同平台账号时，工具调用、结构化输出与加密推理内容原样传递；支持 function/custom 工具和只含客户端函数的 namespace。终止事件在扣费成功后发送，`incomplete` 保留原协议含义；失败响应中的有效 usage 仍结算，思考 token 已包含在输出量中，不重复加算。输入、缓存读、缓存写分别计费，Chat 与 Responses 共用互斥 token 计量。协议字段见 [Responses API](https://developers.openai.com/api/reference/resources/responses/methods/create)。

OpenAI/Kimi/Zhipu/DeepSeek/MiniMax 的 Chat 协议账号也可承接普通 Responses JSON/SSE 请求，复合分组按目标平台判断。转换包括文本/图片/文件输入、函数/自定义工具及结果、additional_tools、明文推理、结构化输出及服务等级；自定义工具转成带 input 字符串的函数，返回时还原。上游强制请求流式 usage 并使用 `store=false`，生成 lite-api 响应 ID；实际 Chat 用量和端点参与原账务。输出事件带顺序号，终态及工具完成事件在结算成功后才发出，length/content_filter 对应 incomplete。托管工具、仅加密推理和自动截断尚不能转换；compact、input_tokens、原生压缩和 WebSocket 仍要求原生 Responses 账号。

Responses HTTP/SSE 也支持 Anthropic 平台及 OpenAI/Kimi/Zhipu/DeepSeek/MiniMax 的 Anthropic 协议账号。直接生成 `/v1/messages`，保留文本/图片/PDF、结构化系统指令、缓存标记、工具结果，工具命名空间/custom/客户端发现复用同一套身份映射；原生内容块按出现顺序转为 Responses 输出。compact、input_tokens、原生 compaction 和 WebSocket 仍要求原生 Responses 账号。

`previous_response_id` 的加密历史保存原生思考、签名、隐藏思考块和工具身份；续接仅使用同一 Key/分组/账号及凭证来源。顶层 instructions 可替换，input 中的系统指令继续保留。`store=false` 不保存历史，外部传入的 reasoning 密文不当作 Anthropic 签名；原生签名不出现在 Responses 内容中。失败或断流中已知 usage 仍结算，输出终态与工具完成事件等待结算成功；实际缓存读写和转换后的 effort 用于计费。该链路已通过本地协议/数据库测试，尚无真实 Anthropic 上游联调。

namespace 的函数在 Chat 请求中映射为 `namespace__name`，超长名截断并附加摘要；响应恢复原 namespace/name。强制 tool_choice、additional_tools、显式历史与 previous_response_id 均使用同一映射。相同定义去重，不同定义或映射名称冲突拒绝；namespace 内的托管工具、嵌套 namespace 及同时填写 tools/children 的歧义声明拒绝。原生 Responses 路径保持合法命名空间声明和输出。

客户端工具发现支持 `type=tool_search`、`execution=client`，与 [OpenAI 工具搜索文档](https://developers.openai.com/api/docs/guides/tools-tool-search) 中的客户端执行模式一致。原生 Responses HTTP/SSE/WS 保留声明及调用；Chat 转换保留 description/parameters/strict，并将调用还原为 `tool_search_call`、`execution=client` 和对象形式的 arguments。流式搜索参数累计到 output_item.done 一并发送，该完成事件等待结算成功。网关不执行客户端工具，也不另收独立搜索费用，仍结算上游文本 usage。

`tool_search_output.tools` 中成功返回的 function/custom/namespace 加入 Chat 下一次工具声明，并随加密续接历史保留；不要求重新声明 tool_search。失败或未完成结果保留为工具输出，不启用发现的工具；纯 output 文本/对象保留为历史，不当作工具定义解析。同一工具定义去重，不同 schema、名称或命名空间冲突拒绝；声明顺序和 JSON 对象键顺序不改变结果。发现的托管工具拒绝。省略 execution 或指定 server 的托管搜索尚未实现；Chat 自定义工具的格式约束仍按 input 字符串函数转换。

转换路径的 `store` 默认 true：Redis 保存 AES-GCM 加密的会话历史，绑定客户端 Key、分组、响应 ID 与上游来源，30 天过期；使用部署密钥派生加密密钥，轮换后旧历史无法解密。顶层 instructions 只影响当前轮，input 内的指令和工具结果保留在续接历史中。历史上限 2 MiB/256 条消息，转换输出上限 16 MiB/4096 项；超限明确失败。`store=false` 不保存续接历史；凭证或协议变化、原账号不可用、缓存失效均拒绝续接，不能切换账号重放。当前通过协议模拟及数据库验证，尚无新增真实上游转换联调。

三个 Responses 前缀均提供 `/compact` 和 `/input_tokens`：压缩按返回 usage 结算，token 计数只验证权限/余额/限额而不扣费，两者不支持流式。原生流式 `compaction_trigger` 会规范为最后一个输入项、补充对应协商头，并保存 `native_compaction_v2` 用量标记。未知子路径拒绝转发。

`previous_response_id` 绑定到原客户端 Key、分组、上游账号及上游凭证/地址，Redis 保存 30 天的关联元数据；缺失、到期或账号停用/轮换后拒绝续接，不切换到另一账号。`store=false` 不建立关联；同幂等键的已完成响应可直接重放。当前只支持通过该网关创建的响应续接。后台 Responses、conversation/item_reference、内置搜索/图片等托管工具仍待对应持久任务、隔离和计费实现，当前明确拒绝，未作为已完成能力。

三个 Responses 前缀的 GET 支持 WebSocket 升级。账号需使用 Responses 协议、OpenAI/Grok 平台，并设置 `extra.openai_apikey_responses_websockets_v2_mode="passthrough"`；默认关闭，`openai_ws_force_http=true` 禁止该路径。复合分组按实际目标平台检查。连接私有且逐轮重新鉴权，每个 `response.create` 复用 HTTP 的额度、路由、计价及结算；同一连接串行执行，可用 `stream_id` 关联客户端事件。`generate=false` 无 usage 的预热不扣费，`store=false` 的续接仅在本连接保留。每 Key 最多 4 条连接、进程 128 条；连接上限一小时，客户端空闲五分钟关闭。连接不接受请求幂等头；已发送的轮次不自动重试。下游断开后最多等待 15 秒收取已产生用量，数据库失败仍由待结算记录恢复。原厂 WebSocket 尚无真实凭证验证，当前验证使用本地协议服务。

`POST /v1/alpha/search`、`/alpha/search`、`/backend-api/codex/alpha/search` 提供独立 JSON 搜索代理。请求必须包含 `model`，搜索命令及扩展字段透传；移除不属于独立搜索的 `prompt_cache_key`、`prompt_cache_retention`、`store`。选择 OpenAI 平台 Chat/Responses 协议的 API Key 账号，上游路径为 `/v1/alpha/search`；复合路由使用 responses 端点并仅允许 OpenAI 目标。该入口不支持流式，不代表任意上游已提供此端点。

alpha 搜索成功一次计一笔 `per_request` 用量，不要求上游 token usage。管理员通过分组 `web_search_price_per_call` 设置 USD/次：未配置默认 0.01，0 为免费，负数清除覆盖值，省略/null 保持。默认值是固定兼容计价规则，不代表上游报价。费用为单价乘本人专属/分组倍率，渠道与分组模型 token 价格不替代搜索单价，渠道模型限制仍生效；金额按既有 10/8 位口径记录和扣减。三种别名共享幂等与账务，失败不记成功消费。401/404/405 可尝试其他合格账号且不改变原账号全局健康状态；429/502/503/504/529 使用既有有限切换，网络结果不明时不重发。

`POST /v1/web_search`、`/v1/x_search` 及根路径别名提供 Grok 独立搜索，仅限 Grok 分组。请求接受 `query`（空缺时使用字符串 `input`），`max_results` 默认为 5、最多 20；X 搜索另接受 `allowed_x_handles`、`excluded_x_handles`、`from_date`、`to_date` 和图片/视频理解开关。使用固定默认模型 `grok-4.6`，经渠道/账号映射后调用 `/v1/responses` 的原生搜索工具；客户端 `model`、`tools` 和 `store` 不控制上游请求。账号可配置 Chat 或 Responses 协议，搜索不建立文本会话粘性。返回 `query/results/provider/max_results`，只收录上游搜索来源或引用标注中的 HTTP(S) URL；模型文本仅可补充已引用 URL 的标题与摘要。

Grok 独立搜索每次成功按 `search_price_per_1k / 1000` 乘用户专属/分组倍率结算，不叠加响应中的 token 用量。管理员在分组创建/更新中配置该字段：默认 5 USD/千次，0 免费，负数清除，省略/null 保留；默认值同样是固定兼容规则。消费模型分别为 `grok-web-search`、`grok-x-search`，实际请求/上游/响应模型另行记录。权限、模型许可、渠道限制、额度、排队、RPM、幂等及失败结算恢复共用网关；web/X 幂等相互独立，同类根路径别名共享重放。上游 401/402/403/429/5xx 最多尝试四个不同账号，网络结果不明不重发。当前由本地协议服务和真实数据库测试验证，尚无 Grok 原厂搜索实测。

`POST /v1/messages` 支持 Anthropic 原生 JSON/SSE，`/v1/messages/count_tokens` 及 `/messages/count_tokens` 别名支持原生 Anthropic 计数及 Gemini 转换计数；别名共用鉴权、模型限制和幂等记录，不写消费账务。按量兼容平台可使用 `credentials.api_protocol=anthropic`。原生转发时版本和 beta 协议头受长度限制后传递，签名、工具调用、缓存控制和内容事件保留。缓存命中、5 分钟/1 小时缓存写入分别计量，`message_delta` 采用累计用量，结算成功后才发送 `message_stop`。

Messages 也可调用 OpenAI/Kimi/Zhipu/DeepSeek/MiniMax/Grok 的 Chat 协议账号，支持 JSON/SSE、系统指令、文本/图片/PDF、函数工具、并行工具结果、结构化输出及停止序列。转换固定 `store=false`，每轮由客户端携带历史；思考明文随工具调用回传，厂商签名和隐藏思考不传给 Chat，缓存标记不伪装为 Chat 缓存控制。复合分组的 `messages` 路由和模型目录使用同一准入规则；`count_tokens` 仍要求原生计数账号。

该转换实时返回文本和思考，工具块在参数校验和结算成功后按顺序发送，再发送 `message_delta/message_stop`；并行工具碎片不会造成重叠的 Messages 内容块。实际 Chat usage 的普通输入、缓存读写及输出分别计费，日志保留实际端点；max 按固定模型兼容规则保留或转成 xhigh，以实际 effort 计价。无等价映射的 top_k、服务端上下文管理及托管工具在派发前拒绝。已通过本地 HTTP 协议及数据库测试，真实上游联调尚未覆盖此转换。事件结构参考 [Messages 流式协议](https://platform.claude.com/docs/en/api/messages-streaming)。

Messages 也可直接调用上述六个平台的 Responses 协议账号，支持 JSON/SSE、系统/图文/PDF、工具及包含图片的工具结果、结构化输出。请求使用 `store=false` 与 `include=["reasoning.encrypted_content"]`，不经过 Chat 格式；工具 ID 与数字精度保持。文本及思考摘要增量返回，工具块和后续内容等结算成功后发送；length/filter 分别成为 max_tokens/refusal，原始 Responses 用量参与扣费。该转换不支持非空 stop_sequences、托管工具或原生 count_tokens。

Responses 思考密文通过 AES-GCM 封装为 Messages 的 signature，绑定当前客户端 Key/分组和上游账号/凭证来源，30 天有效。客户端携带该签名续接时只能选择原来源；跨 Key/组、篡改、到期、部署密钥或上游凭证轮换会拒绝，原账号不可用时不改投。外部厂商签名不作为 Responses 密文转发。服务端不为此保存新会话表或明文历史；相同响应的幂等重放保留原签名。完整 reasoning item 的 ID、summary 和 encrypted_content 用于重放，行为依据 [OpenAI reasoning 文档](https://developers.openai.com/api/docs/guides/reasoning)。已用本地 HTTP 上游和数据库验证，尚未进行真实上游联调。

Messages 也可直接接入 Gemini 原生账号，保留系统/图文/PDF、函数与工具结果、停止序列、top_k、JSON schema、显式思考预算或 effort，内容块顺序保持。缓存控制不伪装为 Gemini 缓存设置，托管工具与无等价含义的控制拒绝。输入用量扣除缓存读，输出包含思考 token；工具块及其后内容、message_stop 等待结算成功，失败中的已知消费仍结算。分组与 composite 的 messages/count_tokens 路由、模型发现采用同一准入。

Gemini 带签名的原生 part 使用现有 AES-GCM 封装，通过 thinking/text/tool_use 块的 `signature` 返回；客户端须原样保留该扩展字段。续接校验内容、工具身份、客户端 Key/组、原账号和来源，不能将签名移到修改后的内容上；原始函数 ID 与结果 ID 配对。导入的无签名工具历史使用兼容标记，其他厂商思考密文不转发。token 计数将完整内容、系统与工具定义放入 `generateContentRequest` 调用上游 [countTokens](https://ai.google.dev/api/tokens)，返回 `input_tokens`，不扣费；上游失败或无效计数不会用本地估算伪装成功。已通过本地协议及数据库验证，真实 Gemini Key 联调仍未覆盖。

Gemini 分组使用 `POST /v1beta/models/{model}:generateContent`、`:streamGenerateContent`（SSE）和 `:countTokens`，模型映射同时应用于 URL 与嵌套 token 计数请求。输出 token 包含思考 token，缓存 token 从普通输入中拆分；原生流保持 Gemini 事件形态，不添加 Chat 的 `[DONE]`。当前原生端点支持文本生成及其多模态输入，媒体生成输出和协议转换继续开发。

`GET /v1/models`、`/models` 及其 `/{model}` 返回当前 Key 分组可见的模型，应用渠道/账号映射和客户端白名单。`/v1beta/models` 及详情返回 Gemini 原生格式，支持 `pageSize`/`pageToken`；上游模型目录按账号及配置版本缓存一分钟，支持上游分页。目录不产生消费，零余额仍可查询，禁用或到期的身份不可查询；ETag 命中也先验证权限。中转没有模型目录接口时可配置账号模型映射，通配映射需有具体目录或白名单模型才能枚举。

`GET /backend-api/codex/models` 或列表请求带 `client_version` 时返回客户端模型 manifest。管理员可在 OpenAI 分组配置 `codex_models_manifest_config`，指定 1–10 个组内账号及 `fallback_to_scheduler`；按配置顺序合并指定目录，缺失能力不虚构上下文大小。`POST /api/v1/admin/accounts/{id}/models/sync-upstream` 同步完整能力到账号元数据，不改模型映射或消费计数；部分元数据返回警告，轮换凭证或地址会清除旧能力快照。复合分组目录根据路由和各账号协议合并可见公共模型，不暴露映射目标，跨平台别名冲突不猜测归属；Gemini 原生列表只展示其可用分支。精确路由可直接枚举，前缀路由需要具体上游模型或白名单条目；查询期间路由变更返回 409，避免返回混合配置结果。

`POST /v1/embeddings` 和 `/embeddings` 支持 OpenAI 分组的向量请求，接受单条/批量文本及 token 序列，保留 `dimensions` 和 `encoding_format=float|base64`。仅选择 Chat/Responses 协议的 API Key 账号；上游模型映射、权限、限额和扣费与文本共用。嵌入请求不支持流式，输入 token 按价卡计费，纯文本嵌入无需配置输出价；缺少有效 usage 不能记成零消费成功。输入形式依据 [OpenAI Embeddings API](https://developers.openai.com/api/reference/resources/embeddings/methods/create)。

网关支持 Bearer、`X-Api-Key` 和 `X-Goog-Api-Key`；Gemini 原生入口还支持兼容的 `key` 查询参数，该参数不会转发到上游。客户端原始鉴权、Cookie 和任意自定义 header 不透传。各 token 计数入口仍检查权限、余额与限额，返回计数而不写消费日志或扣余额。

账号选择考虑优先级、并发、额度、到期及冷却；上游 429/502/503/504/529 在尚未输出时最多尝试三个不同账号。401/403 标记认证错误，429 写入冷却。客户端取消传至上游，结算后释放并发名额。流式请求自动要求 usage，只有完成和结算成功才输出协议对应的结束事件；缺失 usage 返回待核查错误，已收到的 usage 在后续断流时仍结算。

文本会话支持一小时滑动过期的账号粘性。Chat/Responses 优先读取 `Session-Id`、`Session_id`、`Conversation_id` 及兼容会话头，其次 `prompt_cache_key`；缺省时按模型、工具、系统指令和首条用户输入形成摘要。Anthropic 优先使用 `metadata.user_id` 中的新旧会话标识，其次缓存内容和消息摘要。Gemini 使用会话头或模型/系统指令/首条用户输入，Grok 另支持 `X-Grok-Conv-Id` 并按模型隔离。Redis 仅保存摘要、账号 ID 和上游来源指纹，按客户端 Key、分组、平台、协议隔离，刷新发生在实际派发前。

模型优先池优先于普通会话粘性，池内优先使用会话账号；其余准入、价格限制和并发检查仍生效，忙碌或失效可换用其他合格账号。上游 Key、地址或协议变化会使原偏好失效；`previous_response_id` 的强制绑定始终优先，不能借普通粘性改投。token 计数、Embedding 和已完成的幂等重放不创建或刷新粘性。Redis 故障返回 503；粘性是调度偏好，不保证同时发起的首批请求选择同一账号。

并发满载时使用单实例有界等待：每个用户最多排队 20 个请求，每个繁忙账号最多登记 100 个等待者，整个进程最多 128 个；每层最多等 30 秒，满队列或超时返回 429。有其他合格空闲账号时立即调度；全部候选忙碌时按首个候选登记等待，释放槽位后重新选择合格账号。等待者由释放通知或每秒检查唤醒，不保证严格先入先出。Responses 续接仍只等绑定账号。SSE 每 10 秒发送注释心跳；心跳后失败以协议错误事件结束，不输出成功结束标记。

排队期间不占用数据库连接、不扣费、不增加 RPM；派发前检查并计入一次 RPM，账号重试不重复计数。用户出队后重新读取身份、余额、额度和策略；账号等待时还复查分组与复合路由，已捕获策略发生变化则返回 409，要求发起新请求。更改 Key 分组、禁用身份、余额耗尽等不会因排队而绕过准入。取消、超时和停止服务会清理等待计数，已派发请求保留用户及账号槽位至结算结束。

管理员可通过 `GET /api/v1/admin/ops/user-concurrency` 查询活跃用户的当前占用/排队，通过 `GET /api/v1/admin/ops/concurrency` 查询账号及平台/分组汇总，后者支持 `platform`、`group_id` 筛选。数据来自本实例即时计数，不需要启用监控产品；只供管理员查询。共享账号分别计入其各个分组，分组汇总不能直接相加，平台汇总只计一次；容量是配置值，不代表上游健康或此刻可调度。

基础运行查询还包括 `/api/v1/admin/ops/account-availability`、`/realtime-traffic`、`/errors`、`/request-errors`、`/upstream-errors` 及错误详情。可用性检查状态、人工停调、期限、冷却和额度；具体模型和实时并发仍以派发检查为准。流量按原始用量及最终失败请求计算最近 1/5/30/60 分钟 QPS/TPS，重试及已有用量的失败请求不重复计数。HTTP 上游拒绝尝试只进入上游诊断，最终错误可通过 `/request-errors/{id}/upstream-errors` 关联查询；不保存上游错误正文或凭证。错误列表默认最近一小时，可按身份、资源、平台、模型、状态和 RFC3339 时间筛选，最长 31 天。

`/api/v1/admin/ops/ingress-rejections` 按分钟记录网关鉴权拒绝；IPv6 按 /64 归并，不记录尝试的 Key，每秒最多写 100 次。`/ingress-rejections/health` 报告进程内失败与丢弃计数；`/auth-cache-invalidation/health` 明确返回数据库直读模式及 outbox 积压数，不伪装存在缓存订阅者。`/api/v1/admin/groups/usage-summary` 提供精确累计/今日/昨日费用，`/capacity-summary` 提供健康账号的配置并发及当前占用。以上接口仅管理员可用，不启动历史聚合或监控模板任务。

用量、扣费去重、用户余额、Key/账号计数和平台额度同事务写入。日志精度为 10 位，余额与 Key/账号扣款为 8 位；起始余额为正的已发生消费可使余额为负。Redis 保存不含提示词或凭证的待结算记录，后台重试和重启恢复不重复扣费；持久部署须保留 Redis AOF 和 PostgreSQL 数据。上游尚未返回 usage 时的进程崩溃仍需人工核查，不能承诺第三方调用恰好一次。

可选 `Idempotency-Key` 在同一个客户端 Key 下识别重复请求；24 小时内完整 JSON/SSE 响应可重放，同键不同内容返回 409。进程中断遗留的 processing 记录即使过期也拒绝自动重发，需核查原请求；超过 16 MiB 的响应不缓存重放。客户端请求幂等和扣费去重分别记录。

用户通过 `/api/v1/usage`、`/stats`、`/{id}` 和 `/errors` 查询本人原始用量、汇总和错误，管理员通过 `/api/v1/admin/usage`、`/stats` 查询。`GET /v1/billing` 使用客户端 Key 查询余额及额度；余额耗尽仍可查询。管理员通过 `/api/v1/admin/users/{id}/platform-quotas` 的 GET/PUT 配置平台额度，`/reset` 重置指定窗口；用户通过 `/api/v1/user/platform-quotas` 查询。平台额度 NULL 为不限、0 为禁止，日/周按 Asia/Shanghai 自然日/周，月按滚动 30 天。

`GET /v1/usage` 使用客户端 Key 返回 `quota_limited`（有总额度或消费窗口）或 `unrestricted`（钱包余额）视图，以及当前 Key 的今日/累计用量、每日用量和模型汇总。额度耗尽、零余额仍可查询，禁用/删除/到期 Key 和失效分组权限仍拒绝；查询受网关 RPM、用户并发和 10 秒数据库超时约束。5h/1d/7d 为滚动消费窗口，过期或未开始显示零，不改写消费记录。`daily_usage` 的 `days=1–90`、`timezone` 与用户每日查询共用规则；`model_stats` 默认近 30 天，可传 Asia/Shanghai 的 `YYYY-MM-DD` 起止日期，结束日包含全天。非法日期、倒序范围、非法时区返回 400。所有汇总直接读取原始用量，金额保持精确数值；查询参数不能切换 Key/用户，不返回上游账号成本或内部身份。

`GET /api/v1/admin/system/version` 返回当前本地版本，沿用管理员 JWT/机器 Key 鉴权；与公开 `/api/v1/version` 使用同一版本值（当前开发构建为 `dev`），不访问远端更新服务。

Key 列表费用查询保留 `POST /api/v1/usage/dashboard/api-keys-usage`（`api_key_ids` 最多 100 个，只返回本人 Key）：`today_actual_cost` 为 Asia/Shanghai 当日费用，原 `total_actual_cost` 字段为近 30 天费用。`GET /api/v1/user/api-keys/{id}/usage/daily` 支持 1–90 天和显式 `timezone`，默认 30 天、Asia/Shanghai。金额直接在数据库精确汇总，不改变原始消费。

管理员审计查询为 `/api/v1/admin/audit-logs` 和 `/{id}`，支持操作者、动作、方法、IP、RFC3339 时间、成功状态及关键词筛选；每页最多 200 条。查询不提供清空能力。`/api/v1/admin/usage/search-users` 与 `/search-api-keys` 为账务筛选提供用户/Key 简要信息，包含历史用户归属，不返回密码和 Key 原文。

## 异步图片任务

`POST /v1/images/generations/async` 和 `/v1/images/edits/async` 接受与同步图片相同的 JSON 或编辑 multipart 请求，返回 202、任务 ID 和 `poll_url`；通过 `GET /v1/images/tasks/{task_id}` 查询。三个入口均有去掉 `/v1` 的别名。当前使用允许图片的 OpenAI 分组或路由到 OpenAI 的 composite 分组，沿用模型、账号、价格、限额和计费规则。流式图片请求拒绝；`Idempotency-Key` 在同一 Key、相同操作及别名之间重放原任务，不同请求体冲突返回 409。

先由管理员配置 `/api/v1/admin/backups/image-storage`（GET/PUT，POST `/test` 检查桶权限）。仅支持独立 S3 兼容存储，`reuse_backup_s3=true` 拒绝；读取配置不返回 secret，空 secret 保留已有值。开启且配置完整才接受新任务。上游返回的 base64 或 URL 图片经受控网络下载和大小校验后上传，结果只保存对象 URL；关闭存储或余额/Key 额度耗尽后，已生成任务仍可由原 Key 查询，禁用/删除/到期 Key 仍拒绝。对象本身的生命周期由存储管理，任务完成后保留 24 小时；私有 URL 使用配置的签名有效期。

请求及存储配置快照加密持久化在 Redis，不保存客户端 Key 明文；执行前重新检查 Key 归属、分组与权限。单实例一个执行者，最多 32 个待完成任务，队列满返回 429。Redis 应启用持久化且不使用会淘汰任务的策略；需要抵御宿主机断电时使用 `appendfsync always`。运行时先持久结算已发生消费，再转存图片，随后保存可恢复的结果/账务检查点。结算检查点恢复和重复轮询不重复生成或扣费；转存失败仍记录已发生的消费。重启后继续未派发任务和待结算任务；上游请求已派发但结果未持久化时，因上游没有可续接任务 ID，任务明确失败并提示核查，避免自动重发产生重复费用。

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


## Gemini 原生图片

`POST /v1beta/models/{model}:generateContent` 和 `:streamGenerateContent?alt=sse` 支持 Gemini API Key 图片生成及带输入图片的编辑。请求沿用原生 `contents`、`generationConfig.responseModalities` 和 `imageConfig`，模型映射、composite→Gemini、鉴权、并发/RPM、幂等及额度检查沿用网关。输入上限 32 MiB，JSON 响应或单个 SSE 帧上限 16 MiB。原生字段参见 [generateContent](https://ai.google.dev/api/generate-content)。

图片数量优先取实际 `inlineData` / `inline_data` 图片 part；兼容累积式 SSE，以单帧最大图片数计量，纯增量多图流分散在不同帧时可能少计。无内联图片时，对已知图片模型的正常完整响应沿用一张的兼容回落；上游明确拦截或图片失败不触发回落。`candidatesTokensDetails` 中的 IMAGE token 与普通输出分别记录，思考 token 计入普通输出。明确 token 价卡须有真实 token usage，不能用图片数量填造。

计价顺序为：有效的显式 token 价卡优先并使用普通倍率；其他情况依次使用分组模型价卡、分组尺寸价、渠道媒体价、固定图片参考价。`image_price_1k/2k/4k` 单位 USD/张，0 免费、负数清除覆盖、省略/null 保持。按图计价使用 `image_rate_independent` / `image_rate_multiplier`，否则采用有效用户/分组倍率。参考价的 2K/4K 系数为 1.5/2，是兼容基线而非实时原厂报价。

账务尺寸沿用请求 `imageSize` 的 1K/2K/4K，缺省或 AUTO 按 2K；原生 `512` 可转发，但原 schema 无此计费档，沿用缺省 2K 并记录原输入值。该计费口径独立于上游默认输出尺寸，不通过解码像素修改字段含义。原 `image_count`、`image_size`、`image_input_size`、`image_size_source`、图片 token/费用对本人用量查询可见。

计费价格在派发前固定，成功图片即使后续断流仍结算；数据库失败保留账务收据恢复，终止成功帧在结算前不发送。该能力使用本地 HTTP 上游模拟和 Docker PostgreSQL/Redis 验证，尚未使用真实 Gemini 图片凭证联调；Chat/Messages/Responses 转换的图片输出扩展仍需继续验证。

## 批量图片任务

Gemini API Key 分组可启用 `allow_batch_image_generation`，通过 `POST /v1/images/batches` 提交 `model`、`items`（`custom_id`、`prompt`、可选 `output_count` 和 `reference_images`）。当前支持 1K PNG，每项输出 1–4 张，展开后最多 200 项；支持原厂或兼容中转的原生批量接口，不使用 Vertex 身份。上游文件和任务协议依据 [Gemini Batch API](https://ai.google.dev/gemini-api/docs/batch-api)，本地 HTTP 模拟与 Docker 数据库已验证，尚未使用真实批量图片凭证联调。

同一路径 GET 列出任务，`/models` 列出候选模型；`/{id}` 查询或 DELETE 隐藏终态记录，`/{id}/items` 查询条目，POST `/{id}/cancel` 请求取消，GET `/{id}/items/{custom_id}/content` 下载单图，GET `/{id}/download` 下载 ZIP，DELETE `/{id}/outputs` 删除上游结果。同一 Key 的 `Idempotency-Key` 重放原任务，改变请求返回 409；跨 Key 或用登录令牌访问拒绝。列表支持 `limit` 和数值 `cursor` 偏移，条目支持状态筛选。

提交时将可用余额转入 `frozen_balance`；单价快照按 `image_price_1k`（缺省取每图价卡）、有效图片倍率、账号倍率以及 `batch_image_discount_multiplier` 计算，冻结使用 `batch_image_hold_multiplier`，后者不得小于折扣。结果按成功条目结算并释放差额，失败和确认取消释放全部冻结；余额变化、去重、用量和终态在同一事务中写入。批量账务沿用独立冻结语义，不再按同步调用累加 Key/账号消费计数。

PostgreSQL 保存加密请求、任务、结果索引和冻结凭据。单实例每 15 秒顺序处理最多 32 个任务；提交结果不明时通过上游任务列表查找原任务，不盲目重发，不能确认时保留冻结等待核查。重启可继续轮询和结算，价格编辑不改变已接收任务。未派发任务失去权限或账号会取消并释放余额；已派发任务须保留原上游账号和凭证以便恢复。完成后结果保留 72 小时，由 worker 清理上游文件；零余额仍可读取结果。下载会核对持久索引中的身份、数量和 MIME，结果缺失或变化返回错误；ZIP 限制 256 MiB，超限使用单图下载。


## Grok HTTP 语音

`POST /v1/tts`、`POST /v1/stt` 及根路径 `/tts`、`/stt` 接入 Grok API Key 账号。TTS 转发 JSON 并返回原始音频字节和音频 Content-Type；STT 保留 multipart 的 boundary、重复 keyterm 和文件字节，选项必须放在 file 前，也允许兼容中转的 JSON `url` 请求。网关不下载输入音频 URL。客户端 JSON 重复字段会在派发前归一，歧义文本字段拒绝；模型白名单检查显式 model，没有 model 时检查 tts/stt。账号的文本模型映射不限制语音能力，音频请求中的 model 原样交给上游。

复用 Key/JWT 隔离、IP/用户/分组权限、余额/Key/平台额度、并发排队、RPM、账号状态和有限失败切换；仅 Grok 分组提供这两个接口。请求上限 32 MiB，响应上限 16 MiB，单次调用上限五分钟；HTTP 音频完整接收、结算后返回，当前不提供 TTS/STT WebSocket。协议参考 [xAI TTS](https://docs.x.ai/developers/model-capabilities/audio/text-to-speech) 与 [xAI STT](https://docs.x.ai/developers/model-capabilities/audio/speech-to-text)。

管理员创建/更新分组可设置 `audio_tts_price_per_million_chars`、`audio_stt_price_per_hour`，支持原 DECIMAL(20,8) 精度：0 免费，负数清除覆盖，省略或 null 保持已有配置。默认兼容价分别为 15 USD/百万字符、0.10 USD/小时，属于固定计费基线，不代表上游当前报价。渠道/分组模型的 per_request 价卡适用时按音频连续计量单位计费，支持 tts/stt 档位；否则使用音频专用价格，再乘用户专属/分组倍率。TTS 沿用去除首尾空白后的 Unicode 字符数；STT 优先使用上游 duration、duration_seconds、audio_duration 或 usage.seconds，缺少时沿用本次上游请求耗时估算，绝不信任客户端自报时长。缺时长估算具有兼容性局限，应优先使用返回真实 duration 的上游。

`Idempotency-Key` 在两个路径别名间共用；TTS 音频以 Base64 JSON 封装保存在原幂等记录，重放恢复原字节和 Content-Type，不重复派发或扣费。已接收到音频的 TTS 断流仍结算已知输入消费；结算失败返回 503，持久待结算记录可恢复，恢复和错误重放都不再次生成。音频调用记录 per_request 账务及端点，不伪造 token 用量。

本功能以本地真实 HTTP 上游模拟和 Docker PostgreSQL/Redis 验证，尚未使用真实 Grok 语音 Key 联调；Realtime、自定义声音和视频能力见下文。

## Grok Realtime

`GET /v1/realtime` 和 `/realtime` 通过 WebSocket 接入 Grok API Key 账号，模型使用 query 参数（默认 `grok-voice-latest`）。网关只接受下游 API Key，不接受 token 子协议、`Idempotency-Key`、外部会话恢复或第三方 conversation ID；上游连接始终使用账号自己的 API Key。JSON 事件和二进制音频双向转发，客户端会话/响应中的模型、字段大小写和禁用的 resumption 会在转发前校验，凭证、Cookie 和客户端鉴权头不会透传。

Realtime 连接受用户并发、Key/IP/分组权限、模型许可、账号健康、额度、RPM 和单实例 WebSocket 容量限制；连接期间每秒复核权限和账号状态，最长空闲五分钟。观察到音频后按连接分钟数建立 Redis 持久账务检查点，断开或策略错误时冻结最终时长并通过既有 SQL 去重账务结算；进程退出、数据库暂时失败或恢复执行不会重新连接上游或重复扣费。分组可设置 `audio_realtime_price_per_min`，省略时使用固定兼容价 0.05 USD/分钟，0 表示免费，金额仍乘分组/用户倍率。

Realtime 已通过本地 WebSocket 上游模拟、Docker PostgreSQL/Redis 与 race 集成验证，覆盖 JSON/二进制音频、账号切换、权限撤销、上游错误隔离、价格快照和恢复去重；尚未使用真实 Grok 语音 Key 联调。


`/v1/custom-voices` 和 `/custom-voices` 提供 POST 创建、GET 列表，以及 `/{voice_id}` 的 GET/PATCH/DELETE 和 `/{voice_id}/audio` 下载。创建接收单个 `file` 的 multipart 表单；更新保留字段省略/null 的不同含义。接口仅接受 Grok 分组的客户端 Key，沿用余额/额度准入、模型许可（`custom-voices`）、RPM、用户及账号并发；管理操作本身不扣费。文件和响应最多 32 MiB，单次上游请求最多五分钟。

声音使用网关生成的 `voice_…` ID，归属创建它的 Key 和分组；列表只返回该 Key 在网关创建的声音，不暴露共享上游账号的整个声音库。列表支持 `limit`（1–1000）和 `pagination_token`，元数据由创建、详情及修改响应更新。参考音频从原账号下载，不在网关长期存储。归属与源账号凭证指纹保存在无过期时间的 Redis 记录中，因此 Redis 持久卷必须纳入备份。更换 Key 分组、删除账号、轮换上游 Key/地址后，不自动将已有声音转交其他来源。

TTS 使用创建返回的 `voice_id`，会固定到原账号；未知、他人或已删除的自定义声音在转发前拒绝。一个 Key 的声音库固定在同一上游账号，Realtime 在握手前选择该账号，并转换 `session.voice` 和兼容的 `session.audio.output.voice`；账号不可用时不切到其他来源。内置声音仍可直接使用，当前名称依据官方语音列表。管理操作支持 `Idempotency-Key` 和路径别名重放；上游创建结果不确定时保留 processing 记录，重试返回 409，需核查上游后处理，不自动重复创建。

协议依据 [xAI Custom Voices](https://docs.x.ai/developers/model-capabilities/audio/custom-voices) 和 [Voice API](https://docs.x.ai/developers/rest-api-reference/inference/voice)。原厂创建接口要求上游账号具备相应权限；本功能通过本地 HTTP/WebSocket 模拟验证，尚未使用真实 xAI Key 联调。


## 上游余额和调度

管理员可用 `GET /api/v1/admin/cn-providers/accounts/{id}/balance` 查询 Kimi、DeepSeek 按量账号余额。结果包含 `success/persisted`、主币种及完整币种明细，写入账号原有 `extra` 余额快照；金额解析和阈值比较保留十进制精度。手动查询只更新快照。接口分别对接 [Kimi 余额查询](https://platform.moonshot.cn/docs/api/balance) 和 [DeepSeek 余额查询](https://api-docs.deepseek.com/api/get-user-balance)，始终使用账号配置的上游地址和代理；中转须支持相同余额端点，不会将中转 Key 发往其他厂商域名。没有此端点的账号返回明确错误。

后台默认每 10 分钟检查可调度的 Kimi/DeepSeek 按量账号，任一币种余额达到 0.5 即可保持调度，不做汇率换算。全部低于阈值或 DeepSeek 返回不可用时，临时停调两个检查周期；恢复后只清除 `cn_balance_low` 原因的临时停调。失败、无效/超大响应不改变已有快照或健康状态，探测期间的账号编辑和更新的余额失败信号不会被旧结果覆盖。人工禁用、认证错误、其他冷却、RPM 和消费计数不会被余额探测清除。

环境变量 `GATEWAY_CN_PROVIDERS_BALANCE_CHECK_ENABLED`（默认 `true`）、`GATEWAY_CN_PROVIDERS_BALANCE_THRESHOLD`（默认 `0.5`）、`GATEWAY_CN_PROVIDERS_BALANCE_CHECK_INTERVAL_MINUTES`（默认 `10`，允许 1–1440）控制后台检查；关闭后台不关闭管理员手动查询或请求失败保护。单账号 15 秒超时，响应上限 256 KiB，顺序检查且同账号不并发探测。Kimi、DeepSeek、Zhipu、MiniMax 请求返回 402 或余额不足的 429 时，记录临时停调并在现有重试上限内尝试其他账号；普通 429 继续使用原限流冷却。该分支不恢复 Coding Plan。通过本地 HTTP 上游模拟及 Docker PostgreSQL/Redis 验证，未使用真实余额凭证联调。

## 视频与 Seedance

Grok API Key 及路由到 Grok 的 composite 分组支持 `POST /v1/videos`、`/v1/videos/generations`、`/v1/videos/edits`、`/v1/videos/extensions`，均有去掉 `/v1` 的别名。创建使用 JSON，返回网关 `request_id`；GET `/v1/videos/{request_id}` 查询，追加 `/content` 下载。查询及下载也支持 `/videos/generations|edits|extensions/{request_id}` 前缀。创建受原 `allow_image_generation` 开关控制，编辑/扩展要求 `video.url`；不会下载客户端输入视频。单次请求最多 32 MiB，输出下载最多 64 MiB，仅接受视频或二进制响应，不跟随跳转或向内容 URL 发送 API Key。协议依据 [xAI 视频生成](https://docs.x.ai/developers/model-capabilities/video/generation)、[编辑](https://docs.x.ai/developers/model-capabilities/video/editing)及[扩展](https://docs.x.ai/developers/model-capabilities/video/extension)。

管理员分组配置支持 `video_price_480p/720p/1080p`（USD/秒）、`video_model_prices`（模型族 → 分辨率 → USD/秒）、`video_rate_independent` 和 `video_rate_multiplier`。价格省略/null 保持，负数清除平面价格覆盖，0 免费；模型价格 `{}` 清空。分组模型 video 价卡优先，其次模型族/平面视频价、渠道媒体价，最后固定兼容基线。`billing_mode=video` 的 `per_request_price` 表示每秒价格，可用 480p/720p/1080p 区间标签；渠道 per_request/image 仍按每个视频一次计费。默认兼容价：grok-imagine-video 为 0.05/0.07/0.07，1.5 为 0.08/0.14/0.25 USD/秒；不是实时原厂报价。默认共享有效用户/分组倍率，开启独立倍率则使用视频倍率。

Grok 在首次观察到 `done` 且存在 `video.url` 时结算，优先使用上游时长，缺少时使用提交时长（默认 8 秒）；沿用原整数秒口径，小数截断，限制在 1–15 秒。编辑和扩展的精细时长语义依赖上游返回，当前不改变原数据库字段含义。用量写入原 `video_count`、`video_resolution`、`video_duration_seconds` 和 `billing_mode=video`。失败/过期任务不扣费，重复查询和下载不重复扣费。

Seedance 使用 `POST /api/v3/contents/generations/tasks`、GET/DELETE `.../{task_id}`，同样支持 `/v3`、`/v1` 和无前缀。账号须为 `platform=openai,type=apikey`，显式设置 `base_url` 和 `credentials.openai_capabilities=["seedance"]`（也接受布尔对象）。设置能力集合后，文本、Embedding、alpha 搜索按各自能力准入；未设置仍保留原普通接口行为。Seedance 可用显式 OpenAI composite 路由；保留原生多模态 content、草稿及参数，草稿只能引用同一客户端 Key 的已完成草稿，并固定原上游账号。回调和计费工具当前拒绝。成功后按上游 completion_tokens 结算；DELETE 对排队任务请求取消，对已结算终态任务删除上游记录，运行中能否取消由上游决定。

两类任务共用加密 Redis 记录和恢复 worker，无需 schema 变更。记录不保存提示词或上游 Key；保存原用户/Key/分组、上游来源指纹和不可变价格快照。最多 32 个待完成任务，单实例每 15 秒顺序轮询；终态记录保留七天，视频 URL 本身的有效期由上游决定。保留 Redis 持久卷和原 `JWT_SECRET`，待结算任务不自动过期。禁用新调度或额度耗尽不阻止原 Key 读取已创建任务；禁用/删除/到期 Key 仍拒绝。变更上游凭证/地址后需要恢复原来源才能继续轮询。

`Idempotency-Key` 在同一 Key、操作和路径别名间重放创建结果；不同内容返回 409。结算失败先保存检查点，恢复不重新生成、不重复扣费。上游创建结果不明确或接受后持久化失败时不自动重发；同幂等键保留 processing，需人工核查。Grok 当前无取消接口。以上使用本地 HTTP 模拟和 Docker PostgreSQL/Redis 验证，尚未使用真实视频供应商凭证联调。
