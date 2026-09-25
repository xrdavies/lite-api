# 单实例部署与版本回退

lite-api 使用一个应用实例、PostgreSQL 和持久 Redis。前端目录仅占位。生产运行使用 `compose.deploy.yaml`；以下命令在仓库根目录执行，需要 Docker Compose。对外访问由现有 HTTPS 反向代理负责，应用端口仅绑定本机。

## 首次部署

准备只允许部署账户读取的 `.env.deploy`，填写独立随机值；该文件按 `.env.*` 规则忽略。`POSTGRES_PASSWORD` 使用 URL 安全字符，例如随机十六进制字符串。`JWT_SECRET` 至少 32 字节，用于会话和媒体任务加密，后续升级保持原值。

```dotenv
POSTGRES_PASSWORD=填写随机十六进制密码
JWT_SECRET=填写至少32字节的随机密钥
LITE_API_IMAGE=lite-api:release-1
LITE_API_VERSION=release-1
LITE_API_PORT=8080
```

```sh
chmod 600 .env.deploy
docker compose --env-file .env.deploy -f compose.deploy.yaml build app
docker compose --env-file .env.deploy -f compose.deploy.yaml up -d --wait postgres redis
docker compose --env-file .env.deploy -f compose.deploy.yaml run --rm --no-deps app init-db
```

通过部署工具将 `ADMIN_EMAIL` 和 `ADMIN_PASSWORD` 临时注入当前进程环境，然后初始化管理员。无需将管理员密码写入 Compose 文件或应用长期运行环境。

```sh
docker compose --env-file .env.deploy -f compose.deploy.yaml run --rm --no-deps -e ADMIN_EMAIL -e ADMIN_PASSWORD app bootstrap
unset ADMIN_EMAIL ADMIN_PASSWORD
docker compose --env-file .env.deploy -f compose.deploy.yaml up -d --no-build app
docker compose --env-file .env.deploy -f compose.deploy.yaml exec -T app /lite-api healthcheck
```

`init-db` 只接受空库，`bootstrap` 只接受尚无用户的数据库，运行 `serve` 不执行 DDL。应用以非 root 用户、只读根文件系统运行。服务使用数据库会话锁拒绝第二实例，因此发布期间应先停止旧应用再启动新应用。

管理员登录后配置分组、明确的模型价格、上游 API Key 账号，再创建用户并调整余额。用户密码登录后自行创建调用 Key。账号配置、模型映射和协议参见 [README](README.md)；文本价格预检需要覆盖输入、输出及适用缓存单价，明确免费时填 0。

## 验证部署与故障恢复

```sh
DOCKER_CONTEXT=desktop-linux python3 scripts/test-deployment.py
```

使用其他 Docker 环境时省略或修改 `DOCKER_CONTEXT`。脚本仅使用 Python 标准库，构建当前工作树及 `HEAD^` 的容器镜像；使用独立 Compose 项目、随机凭证、临时端口和新数据卷，结束后清理。需要 Git 历史中的上一提交，以及镜像构建所需网络或缓存。不会读取部署的 `.env`、连接现有业务库或调用收费上游。

脚本验证：

- 空库初始化、重复初始化拒绝、非 root/只读运行、健康检查、第二实例拒绝。
- 管理员建户、用户建 Key、JSON/SSE、余额调整与跨别名幂等重放。
- 8 并发、40 请求的本地模拟上游样本，JSON/SSE 各半；分别记录直连与网关的响应头、首字节、完成耗时 p50/p95、样本吞吐、容器内存及 Docker CPU/内存资源。记录 PostgreSQL 连接、事务、缓冲块、读写行和死锁计数差值，检查无死锁及未遗留媒体/响应待处理记录。数据库计数也包含观察查询和后台工作，属于累计抽样而非峰值压力；此短样本不构成生产容量上限。
- SQL 故障下消费不提交，Redis 保留待结算凭据；同时保留运行中的 Seedance 任务。
- 对应用、Redis、PostgreSQL 执行 SIGKILL，原卷恢复后用上一提交镜像结算；检查会话、价格快照、余额/Key/用量及上游调用次数。
- 回退后再次升级，不重复生成、扣费或管理员充值，成功请求仍能幂等重放。
- 重新升级后通过注册扩展路径创建 programmatic 后台任务，撤销扩展配置、修改价格并制造 SQL 故障，再次强杀应用/Redis/PostgreSQL；由当前版本恢复原路径/原价一次结算，核对 Key 隔离、程序结果、恢复配置后的别名幂等重放和无声明续接。同时检查上游请求 ID 仍来自创建响应头，创建后清除账号头名配置、轮询和强杀恢复均不替换它，后续新请求写入 NULL。该任务不交给不认识 programmatic 标记的旧版本。
- 在只读镜像中使用编译内置词表执行 Grok 无账号计数、DeepSeek 本地计数和别名幂等重放；上游调用次数、余额和消费记录保持不变。

此脚本补充 `scripts/test-integration.py` 的 Docker/race 业务回归。后者使用临时 MinIO 验证 S3 签名、图片转存/下载、存储故障与账务恢复；两者均不能代替真实模型供应商、各云存储配置联调或全量容量测试。

## 发布与回退

1. 使用不同镜像标签构建新版本，并保留旧镜像和对应部署配置。在独立环境先运行集成和部署验证。
2. 通过现有反向代理暂停新请求，停止旧应用，等待在途请求结束。应用会停止准入并等待后台任务关闭；超过 Compose 的 60 秒宽限期会被终止。
3. 保持同一 Compose 项目名、数据卷和 `JWT_SECRET`，只切换 `LITE_API_IMAGE`。启动新应用，检查健康、管理员版本接口 `/api/v1/admin/system/version`、日志、原始用量及待处理任务后恢复流量。
4. 回退时再次暂停流量、停止应用，把镜像标签改回已验证版本，再启动和检查。正常应用回退不恢复旧数据库快照，也不删除数据卷。

```sh
docker compose --env-file .env.deploy -f compose.deploy.yaml stop app
# 编辑 .env.deploy，将 LITE_API_IMAGE 设置为已构建的新版本或已验证的旧版本。
docker compose --env-file .env.deploy -f compose.deploy.yaml up -d --no-build --force-recreate app
docker compose --env-file .env.deploy -f compose.deploy.yaml exec -T app /lite-api healthcheck
```

schema 保持不变不等于所有版本都可任意回退：Redis 中的任务、结算凭据与加密格式也必须兼容。以实际版本组合运行部署验证；有新增任务类型的版本应先完成任务或确认旧版本可恢复它。不要用 `down --volumes` 发布或回退。

上游请求 ID 修正后，视频和后台 Responses 任务单独保存创建响应头标识。旧版可以忽略新增字段并继续账务恢复，但会恢复其原来的错误标识语义；要求请求标识准确时，应由支持该字段的版本完成这些任务。新版本读取没有该字段的旧任务时保持 NULL，已经生成的旧结算检查点和已结算历史行不回填。

Responses 注册扩展沿用原后台任务结构的 Operation 字段保存创建后缀；旧任务字段为空时仍按普通创建计费。回退到不支持扩展的版本前，应先完成扩展后台任务，否则旧版可能把未生成收据的任务入口记为普通 `/v1/responses`。`responses_extension_paths` 清空仅阻止新请求，已接受任务由支持扩展的版本继续查询和原价恢复。

全局 Fast/Flex 规则沿用 `settings.openai_fast_policy_settings`，不会新增任务格式；已接受的后台任务继续按持久化档位结算。旧版本不执行这些规则，依赖 block/filter/force_priority 的流量应保持在支持该策略的版本。WebSocket 使用连接建立时的策略快照，需要立即应用新规则时应让客户端重连。

启用远程 MCP 后，响应关联与后台任务含 MCP 标记，用于无声明续接的脱敏与禁止重试。回退到不识别该标记的版本前应停止相关调用并完成后台任务，不能让旧版本继续处理仍有效的 MCP 会话；保留当前版本处理这些会话，或等其归属记录到期后再回退。

Code Interpreter 和托管 Shell 在响应关联及后台任务中共用工具标记与容器归属；托管 Shell 的 skill 引用按账号 `extra.response_skills` 绑定分组及上游来源，inline skill 不写入 Redis。回退到不识别这些字段的版本前须停止相关调用、结束 WebSocket 并完成后台任务；旧版本不能继续承接仍有效的代码工具会话。SQL schema 不变，容器本身的过期和数据保留由上游负责。

## 数据恢复与故障处理

PostgreSQL 卷、Redis 卷和 `JWT_SECRET` 都是持久状态。Redis 使用 AOF `appendfsync always` 和 `noeviction`，包含会话、待结算凭据及加密媒体任务；不能当作可随时清空的缓存。外部备份应同时覆盖两类数据卷、密钥及配置；采用停写冷快照时，先停止应用，再停止数据库和 Redis，使用部署平台对两个卷取快照，之后依次启动依赖与应用。

恢复快照前停止应用，并将两个数据卷及原密钥恢复到同一备份时点，使用对应兼容镜像启动。快照之后发生的上游消费和任务须独立核对；恢复旧快照不能承诺这些消费自动找回。发生数据库连接锁丢失时，应用停止准入，需要恢复数据库后重启应用。

结算恢复使用持久凭据及数据库去重，不会重新发送生成请求。创建已到达上游但结果不明、或者消费同时无法写入 PostgreSQL 和 Redis 的情况需要人工核查供应商记录；不要删除 `processing` 幂等记录或以新幂等键盲目重发。媒体供应商凭证或地址变更时，应恢复原来源再核对已有任务，不能改派另一账号查询。

文件搜索在响应关联和后台任务中保留已授权的向量库 ID，账号 extra 保存按分组及上游来源绑定的授权。回退到不识别这些字段的版本前须停止文件搜索调用、结束相关 WebSocket 并完成后台任务，旧版本不能承接仍有效的搜索续接。账号 API Key、地址或协议变更后需重新提交授权；更换版本本身不重绑授权。

文件输入在响应关联及后台任务中新增FileIDs元数据，账号extra中的response_files独立于向量库授权。回退到不识别文件授权的旧版前，停止所有文件引用调用并排空关联后台任务、关闭相关WebSocket；旧版不能继续这些会话，也不能承接依赖文件分组隔离的请求。

容器网络域名密钥使用现有托管代码标记触发后续请求和资源查询的错误脱敏。启用该能力后，回退前应停止使用域名密钥的调用、完成后台任务并结束相关会话；旧版即使识别容器归属，也可能不清理供应商回显的 domain_secrets。

Programmatic 调用在响应关联、输出条目索引和后台任务中新增工具标记与程序摘要，用于原来源续接、历史完整性校验、错误脱敏和禁止重试。回退到不识别该标记的版本前，应停止相关新调用、完成后台任务并结束会话；旧版本不能继续处理仍有效的程序会话。SQL schema 不变，摘要不保存程序代码或 fingerprint 明文；后台最终结果仍按现有加密任务规则保存。
