# lite-api

面向企业和团队内部使用的 AI 网关。

新建库定义见 [schema/baseline.sql](schema/baseline.sql)，来源版本及 migration 校验和见 [schema/source.json](schema/source.json)。仅移除两张插件表 `sub2api_plugin_installations`、`sub2api_plugin_bindings` 及其专属对象，其余业务表保持原结构和含义。该 SQL 只用于初始化空库。

重新生成快照：`DOCKER_CONTEXT=desktop-linux python3 scripts/export-schema.py ../sub2api/backend/migrations`。脚本校验固定 migration，在一次性 Docker 数据库中生成 schema，不导出业务数据。
