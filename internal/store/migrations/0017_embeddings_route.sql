-- 向量化新增第六条线协议 openai-embeddings，它的出站地址是厂商路由里的
-- (厂商, openai-embeddings) 一行。种子只给两家：openai 的 /v1/embeddings，与 ollama 的
-- OpenAI 兼容层（同一台服务上的 /v1/embeddings）。其余厂商要不要这条路由以它的文档为准，
-- 用 provider set <厂商> openai-embeddings=/v1/embeddings 声明；没声明这一行的厂商在
-- /v1/embeddings 入站上会拿到一条说明该声明路由的 400，而不是被猜一个地址去打。
--
-- models_path 一律留空：这两家的模型列表都在 /v1/models 上，已经由 openai-chat 那条 route
-- 声明过；向量化路由没有自己的列表接口，读取时回退到同厂商其它协议。
-- INSERT OR IGNORE 让已经手工声明过这条路由的库保持原值。
--
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 17。

INSERT OR IGNORE INTO provider_routes (provider, protocol, path, models_path) VALUES
  ('openai', 'openai-embeddings', '/v1/embeddings', ''),
  ('ollama', 'openai-embeddings', '/v1/embeddings', '');
