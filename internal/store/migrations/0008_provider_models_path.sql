-- 厂商级模型列表路径覆盖：providers.models_path 非空时，`model sync` 用 base_url 拼上它，
-- 不看每条路由自己的 models_path；空串表示不覆盖，按路由取。
-- 每条路由的 models_path 仍留在 provider_routes 上。推导只是写入时的默认值（openai-chat 路由
-- 的 /chat/completions 换成 /models），不是读取时的唯一来源：内置 anthropic 厂商只有
-- anthropic 一条路由，没有 openai 路由可推导，读时推导会让 anthropic 型账号丢掉模型列表来源。
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 8。
ALTER TABLE providers ADD COLUMN models_path TEXT NOT NULL DEFAULT '';
