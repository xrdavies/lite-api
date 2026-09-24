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
- 8 并发、40 请求的本地模拟上游样本，输出中位数、p95 和容器内存；此样本不构成生产容量上限。
- SQL 故障下消费不提交，Redis 保留待结算凭据；同时保留运行中的 Seedance 任务。
- 对应用、Redis、PostgreSQL 执行 SIGKILL，原卷恢复后用上一提交镜像结算；检查会话、价格快照、余额/Key/用量及上游调用次数。
- 回退后再次升级，不重复生成、扣费或管理员充值，成功请求仍能幂等重放。

此脚本补充 `scripts/test-integration.py` 的 Docker/race 业务回归，不能代替真实供应商协议验证、对象存储故障测试或全量容量测试。

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

启用远程 MCP 后，响应关联与后台任务含 MCP 标记，用于无声明续接的脱敏与禁止重试。回退到不识别该标记的版本前应停止相关调用并完成后台任务，不能让旧版本继续处理仍有效的 MCP 会话；保留当前版本处理这些会话，或等其归属记录到期后再回退。

Code Interpreter 同样在响应关联及后台任务中保存工具标记与容器归属。回退到不识别这些字段的版本前须停止相关调用、结束 WebSocket 并完成后台任务；旧版本不能继续承接仍有效的代码工具会话。SQL schema 不变，容器本身的过期和数据保留由上游负责。

## 数据恢复与故障处理

PostgreSQL 卷、Redis 卷和 `JWT_SECRET` 都是持久状态。Redis 使用 AOF `appendfsync always` 和 `noeviction`，包含会话、待结算凭据及加密媒体任务；不能当作可随时清空的缓存。外部备份应同时覆盖两类数据卷、密钥及配置；采用停写冷快照时，先停止应用，再停止数据库和 Redis，使用部署平台对两个卷取快照，之后依次启动依赖与应用。

恢复快照前停止应用，并将两个数据卷及原密钥恢复到同一备份时点，使用对应兼容镜像启动。快照之后发生的上游消费和任务须独立核对；恢复旧快照不能承诺这些消费自动找回。发生数据库连接锁丢失时，应用停止准入，需要恢复数据库后重启应用。

结算恢复使用持久凭据及数据库去重，不会重新发送生成请求。创建已到达上游但结果不明、或者消费同时无法写入 PostgreSQL 和 Redis 的情况需要人工核查供应商记录；不要删除 `processing` 幂等记录或以新幂等键盲目重发。媒体供应商凭证或地址变更时，应恢复原来源再核对已有任务，不能改派另一账号查询。

文件搜索在响应关联和后台任务中保留已授权的向量库 ID，账号 extra 保存按分组及上游来源绑定的授权。回退到不识别这些字段的版本前须停止文件搜索调用、结束相关 WebSocket 并完成后台任务，旧版本不能承接仍有效的搜索续接。账号 API Key、地址或协议变更后需重新提交授权；更换版本本身不重绑授权。
