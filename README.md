# lite-api

面向企业和团队内部使用的 AI 网关，使用 Go、PostgreSQL 和 Redis，单实例部署。管理员创建账户，用户管理自己的 API Key。内置前端目录保留占位，当前开发后端。

目前已实现空库初始化、结构校验、首个管理员初始化、密码登录与令牌刷新/撤销、用户管理、分组基础配置/授权、用户 API Key 管理及管理员余额调整、上游 API Key 账号与代理管理、文本手动测试和定时测试计划、渠道价格配置和模型广场。已接入 Chat Completions、Responses、Anthropic Messages 和 Gemini 原生 JSON/SSE 网关、token 计数、用量与事务扣费、平台额度和基础查询；已支持 Responses WebSocket、Chat/Responses 双向基础转换、alpha 和 Grok 独立搜索，以及 OpenAI 兼容图片生成/编辑（JSON URL/data URL 与 multipart 文件）和持久异步图片任务接口；批量任务、其他媒体、托管工具及扩展运行功能仍在开发中。

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

复合分组使用 `platform=composite`，可关联八种已支持平台的 API Key 账号。管理员通过 `/api/v1/admin/groups/{id}/composite-routes` 的 GET/POST 和 `/{route_id}` 的 PUT/DELETE 管理路由，POST 成功返回 201，PUT 为整条替换；`/preview` 接受 `model`、`endpoint`，只预览配置决策，不代表上游当前可用。路由包含 `public_model`、`match_type=exact|prefix`、`target_platform`、`upstream_model`、`endpoint`、`priority`、`enabled`、`notes`；endpoint 支持 any/messages/count_tokens/responses/chat_completions/embeddings/images/gemini；images 当前调度 OpenAI 兼容图片生成/编辑。

复合路由按精确匹配、指定端点、最长前缀、priority 升序、ID 升序选择。空 `upstream_model` 在 exact 时采用 public_model，prefix 时透传具体请求模型。无显式命中时，先使用账号精确模型映射确定归属，多平台争用同一别名则拒绝；再识别已知厂商模型前缀，未知名称拒绝。路由选择平台后，依次应用渠道和账号模型映射。客户端白名单在改写前校验，requested 计价和日志保留公共模型名；平台额度按解析出的具体平台检查、结算。不会跨平台重试，Responses 续接仍绑定原账号和上游来源。当前覆盖已有原生文本/Responses、计数及 Embedding 路径；协议转换与媒体继续开发。

分组可配置 `claude_code_only` 和 `fallback_group_id`。Messages 使用 CLI User-Agent、必要请求头、system 特征和 metadata 识别客户端，兼容单 token 探测及 count_tokens 辅助请求。这是客户端分类规则，所有请求仍必须通过 API Key 鉴权。非匹配客户端在配置 fallback 时使用目标组账号，未配置返回 403；Chat、Responses、Embedding 入口在开启限制时直接拒绝。模型发现和用量查询仍属于原 Key 分组。

fallback 仅委托调度：原 Key 归属、模型白名单、用户价格/倍率、RPM 和用量 group_id 保持原分组；目标组账号必须满足平台、协议、健康、并发、额度和目标渠道模型限制。管理员可指定私有目标组，但用户不会因此获得在目标组建 Key 的权限。平台额度继续按原具体平台计量，原分组为 composite 时按解析平台计量。目标组停用/删除立即停止新派发；排队期间关系或策略变化会重新检查并拒绝旧请求，在途消费按开始时的价格结算。fallback ID 省略/null 保持，0 或负数清除；配置拒绝自引用、循环、受限目标及范围外资源。上游 HTTP 400/503 不触发跨分组 fallback。

分组可配置 `model_routing_enabled` 和 `model_routing`，例如 `{"gpt-*": [12, 18]}`。规则匹配渠道改写后的模型名，区分大小写，精确项优先，其次最长尾部 `*` 前缀；空列表不形成优先池。规则用于 OpenAI、Anthropic 目标平台，复合分组按解析平台执行。优先池内仍按账号优先级和最后使用时间选择，数组顺序不代表优先级；不可用时回退同组其他合格账号，所有权限、协议、额度、并发和渠道模型限制继续生效。规则中的未关联、已删除或其他平台账号不会被调用。省略或 `null` 保留配置，`{}` 清空；开关关闭时保留规则。Responses 续接优先遵守原账号绑定，原账号不可用时拒绝转投。

未固定模型目录账号时，manifest 的能力取模型优先池内账号的交集；优先账号无可用元数据时不借用普通候选的能力。复合分组同名模型在其他端点平台可调用时，仍以共同能力为限。显式 `codex_models_manifest_config` 保持所选目录账号的顺序。目录展示不承诺此刻有并发空位，也不保证回退账号拥有相同上下文上限。

OpenAI、Anthropic 和复合分组可配置 `max_reasoning_effort`、`max_reasoning_effort_over_limit= downgrade|deny` 和 `reasoning_effort_mappings`。上限支持 minimal/low/medium/high/xhigh/max，Anthropic 不接受 minimal；复合分组派发到 Anthropic 时将 minimal 策略目标适配为 low。映射格式为 `{"from":"max","to":"high","match_type":"prefix","model":"gpt-"}`，最多 64 条；from 另可为 none，to 另可为 deny。匹配客户端原始模型名，精确优先于最长前缀/后缀，再到全局；同级按数组顺序，仅执行一次映射，随后执行上限。超限 deny 或映射 deny 返回 403，不调用上游。

策略处理显式 `reasoning.effort`、`reasoning_effort`、`output_config.effort`，保留嵌套其他字段；缺省值不补写，未知值交由上游处理。Chat、Responses、Messages 及其复合派发均使用实际转发 effort 的价格倍率；用量中的 `requested_reasoning_effort` 单独保留规范化的客户端请求值（未知/none 为 null，缺省时可记录模型名的已知后缀）。在途修改策略不改变该次结算，已完成的幂等请求仍可原样重放。省略/null 保留配置，空上限取消限制，空映射数组清除规则；策略不用于 Gemini 或其他具体平台。

## 上游与健康测试

管理员通过 `/api/v1/admin/accounts` 配置 `platform`、`type=apikey`、`credentials.api_key`、可选 `credentials.base_url` 和 `group_ids`。支持的账号平台为 openai、anthropic、gemini、grok、kimi、zhipu、deepseek、minimax；仅接受按量 API Key，账号查询不会返回原始 Key。`POST /api/v1/admin/accounts/{id}/test` 使用 `model_id` 发起真实文本请求，返回测试 SSE；它可能产生上游费用，不计入内部用户消费。

`/api/v1/admin/scheduled-test-plans` 支持创建、编辑、删除，`/{id}/results` 查询结果，`/api/v1/admin/accounts/{id}/scheduled-test-plans` 列出账号计划。这组接口沿用直接 JSON 响应。五字段 cron 默认 UTC，可用 `CRON_TZ=Asia/Shanghai` 指定时区。单实例每 15 秒扫描，按到期顺序执行，每次调用最多 45 秒；`max_results` 控制留存，`auto_recover` 仅恢复可恢复状态，保留人工停调和消费额度。进程停止会取消正在执行的请求，未完成计划下次启动继续检查。

代理支持 HTTP、HTTPS、SOCKS5/SOCKS5h 和到期后的明确备用/直连配置。上游重定向不跟随，TLS 验证不能关闭，公开上游必须使用 HTTPS。内网模型或代理须在部署环境 `UPSTREAM_PRIVATE_CIDRS` 中明确允许目标网段；默认拒绝本机和私网。不要将该配置扩大为任意地址。

独立上游验证无需数据库：设置本地 `UPSTREAM_BASE_URL`、`UPSTREAM_API_KEY`、`UPSTREAM_MODEL` 后执行 `./bin/lite-api upstream-check`。命令发起一次 JSON 和一次 SSE 文本生成，仅输出状态和时延摘要。Go 客户端对指定中转和 `gpt-5.6-luna` 的 JSON/SSE 已实测成功；其他平台当前由协议模拟测试覆盖，不能据此宣称所有原厂模型均已联调。

## 渠道与定价

管理员通过 `/api/v1/admin/channels` 管理渠道、分组关联、模型映射、价格和账号成本规则。一个分组只能属于一个渠道；关联与价格替换在同一事务中完成。价格沿用各字段的十进制精度，token 价格单位为 USD/token，支持科学计数法。`billing_model_source` 可配置 requested、channel_mapped、upstream 或 response_model。Chat Completions 已使用请求开始时的价格快照结算，消费期间修改价格不会回改该次费用。

价格支持 token、per_request、image，缓存读写及 1h 写入价、上下文阶梯、时段/服务等级/推理倍率。上下文阶梯按 `(min_tokens, max_tokens]` 匹配；时段使用显式时区与 `[start_time, end_time)`，结束 `00:00` 表示当天结束。账号成本规则单独保存，不改变用户价格。渠道缺失的文本单价回落到参考价，显式零价仍为免费；单独配置缓存写入价同时覆盖 5m/1h，单独的 1h 价优先。渠道阶梯替代参考阶梯，未配阶梯则继承参考长上下文倍率；渠道自定义价不叠加供应商默认时段策略。`restrict_models=true` 仍要求模型命中渠道价卡，不能通过参考价绕过限制。

管理员创建或更新分组时可设置 `model_pricing`，格式与渠道价卡相同；省略或 `null` 保留现值，`[]` 清空。分组价按选定的计费模型名匹配，精确名优先于首个通配项；价卡平台标签不限制分组内的名称匹配。匹配后整张替代渠道卡，缺省单价继承参考价，显式零价生效；不能绕过渠道模型限制。分组 token 卡只覆盖基础价，所存自定义区间不参与 token 计费，长上下文使用参考阶梯并受分组开关控制；按次/图片卡保留自身档位。分组价不支持时段配置；订阅专属的高峰分组倍率不在按量业务中启用。网关请求固定分组价快照，模型广场使用相同解析规则；可用渠道查询展示渠道价，分组实际价格以模型广场为准。

内置 [参考价表](internal/app/reference_prices.json) 是截至 2026-09-23 的固定兼容计价基线，包含 248 条价卡及八个平台，不代表上游当前实际售价或全部协议已完成。`GET /api/v1/admin/channels/model-pricing?model=...` 查询参考价（可附 `platform`），`GET /api/v1/admin/channels/pricing/sync-models?platform=...` 枚举当前目录，均返回 `as_of` 和 SHA256；后者不发起联网更新。参考价支持明确的模型 ID、日期后缀和 GPT 推理等级后缀，不按未知名称猜价。中转跨品牌模型可使用唯一匹配的参考价；未知或冲突的身份需配置渠道价。

部署可用 `PRICING_FILE` 指定同格式的完整本地价表；为空时使用内置表。文件上限 8 MiB、10000 条价卡，包含 `as_of`、`source`、`prices`；单价单位 USD/token，模型名必须具体，token 阶梯用倍率表示。目录独有的 `fast_ratio` 支持精确分数（如 `"5/3"`），显式 `fast_multiplier` 优先；展示同时提供精确分数字段。每分钟校验变更后原子发布，建议通过临时文件加 rename 更新。启动时无效文件会拒绝启动；运行中无效或丢失文件保留上一份有效价表并记录错误。单次请求及账号重试共用固定目录快照，更新不改变已开始请求的结算。

`PUT /api/v1/admin/settings` 支持 `site_name`、`available_channels_enabled` 及模型广场开关。可用渠道默认关闭，开启后用户可通过 `/api/v1/channels/available` 查询可访问分组下的具体模型和价格，包含映射别名和本人倍率；私有分组、其他平台模型、内部账号成本规则及映射目标不向无权限用户返回。

`GET /api/v1/model-plaza` 是分组模型价格目录，由 `model_plaza_enabled`（默认关闭）、`model_plaza_require_auth` 和 `model_plaza_description` 控制。匿名只见公开分组；使用登录令牌可查询已授权专属分组和本人的 `user_rate_multiplier`，受公开分组限制的用户仍需授权。无效令牌明确拒绝，API Key 不能代替登录。目录每个 IP 每分钟最多 60 次，响应禁止缓存；不调用上游或产生消费，也不保证列出的模型当前可调度。

广场价格单位为 USD/token，倍率另列。token 阶梯展示绝对单价，与扣费共用解析规则，包括缓存 5m/1h、阶梯空档回落、显式零价、时段/推理倍率；关闭分组长上下文计费后展示基础档。渠道的图片/按次价和档位保留。缺价返回 `null`；按上游/响应模型决定价格时，也不预报未经确认的价格。`official_pricing` 返回可识别模型的固定参考价，目录附 `pricing_as_of` 和 `pricing_checksum`；别名不猜测官方身份。分组媒体专用价格仍待后续实现。

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

alpha 搜索成功一次计一笔 `per_request` 用量，不要求上游 token usage。管理员通过分组 `web_search_price_per_call` 设置 USD/次：未配置默认 0.01，0 为免费，负数清除覆盖值，省略/null 保持。默认值是固定兼容计价规则，不代表上游报价。费用为单价乘本人专属/分组倍率，渠道与分组模型 token 价格不替代搜索单价，渠道模型限制仍生效；金额按既有 10/8 位口径记录和扣减。三种别名共享幂等与账务，失败不记成功消费。401/404/405 可尝试其他合格账号且不改变原账号全局健康状态；429/502/503/504 使用既有有限切换，网络结果不明时不重发。

`POST /v1/web_search`、`/v1/x_search` 及根路径别名提供 Grok 独立搜索，仅限 Grok 分组。请求接受 `query`（空缺时使用字符串 `input`），`max_results` 默认为 5、最多 20；X 搜索另接受 `allowed_x_handles`、`excluded_x_handles`、`from_date`、`to_date` 和图片/视频理解开关。使用固定默认模型 `grok-4.6`，经渠道/账号映射后调用 `/v1/responses` 的原生搜索工具；客户端 `model`、`tools` 和 `store` 不控制上游请求。账号可配置 Chat 或 Responses 协议，搜索不建立文本会话粘性。返回 `query/results/provider/max_results`，只收录上游搜索来源或引用标注中的 HTTP(S) URL；模型文本仅可补充已引用 URL 的标题与摘要。

Grok 独立搜索每次成功按 `search_price_per_1k / 1000` 乘用户专属/分组倍率结算，不叠加响应中的 token 用量。管理员在分组创建/更新中配置该字段：默认 5 USD/千次，0 免费，负数清除，省略/null 保留；默认值同样是固定兼容规则。消费模型分别为 `grok-web-search`、`grok-x-search`，实际请求/上游/响应模型另行记录。权限、模型许可、渠道限制、额度、排队、RPM、幂等及失败结算恢复共用网关；web/X 幂等相互独立，同类根路径别名共享重放。上游 401/402/403/429/5xx 最多尝试四个不同账号，网络结果不明不重发。当前由本地协议服务和真实数据库测试验证，尚无 Grok 原厂搜索实测。

`POST /v1/messages` 支持 Anthropic 原生 JSON/SSE，`/v1/messages/count_tokens` 支持原生 Anthropic 计数及 Gemini 转换计数；按量兼容平台可使用 `credentials.api_protocol=anthropic`。原生转发时版本和 beta 协议头受长度限制后传递，签名、工具调用、缓存控制和内容事件保留。缓存命中、5 分钟/1 小时缓存写入分别计量，`message_delta` 采用累计用量，结算成功后才发送 `message_stop`。

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

账号选择考虑优先级、并发、额度、到期及冷却；上游 429/502/503/504 在尚未输出时最多尝试三个不同账号。401/403 标记认证错误，429 写入冷却。客户端取消传至上游，结算后释放并发名额。流式请求自动要求 usage，只有完成和结算成功才输出协议对应的结束事件；缺失 usage 返回待核查错误，已收到的 usage 在后续断流时仍结算。

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
