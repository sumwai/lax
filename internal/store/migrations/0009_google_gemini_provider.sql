-- gemini 内置厂商进库：`GET /v1beta/models` 拉回来的模型名带 `models/` 前缀，转发路径里的模型名
-- 是裸名，所以模型列表路径与转发路径要分开写两条：列表在 `/v1beta/models`，转发在
-- `/v1beta/models/{model}:generateContent`。模型列表按页返回（pageSize / nextPageToken），
-- 翻页与去前缀都在 model sync 里做，这里只种地址。
--
-- 写法与 0006 一致：厂商行只放 base_url、备注、默认协议与厂商级列表覆盖，路径细节全在
-- provider_routes 上。gemini 路由的 models_path 要显式给：写入侧的默认值只推导 openai-chat
-- 一条，gemini 自己的列表路径没有推导规则。
--
-- 用 INSERT OR IGNORE 而不是 INSERT：升级时用户可能已经用 `provider add google-gemini`
-- 自己建过这家厂商（名字撞上、地址指向代理），那两行原样保留，迁移不覆盖用户的写法，
-- 也不因为主键冲突整笔失败。
--
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 9。
INSERT OR IGNORE INTO providers (name, base_url, note, default_protocol, models_path)
  VALUES ('google-gemini', 'https://generativelanguage.googleapis.com', '', 'gemini', '');

INSERT OR IGNORE INTO provider_routes (provider, protocol, path, models_path)
  VALUES ('google-gemini', 'gemini', '/v1beta/models/{model}:generateContent', '/v1beta/models');
