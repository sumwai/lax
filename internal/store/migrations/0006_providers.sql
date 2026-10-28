-- 厂商与路由进库：同一家上游的不同协议路径并不一样（deepseek 的 chat 在 /v1/chat/completions，
-- anthropic 在 /anthropic/v1/messages；commandcode 的 chat 在 /chat/completions，anthropic 在
-- /messages，模型列表在 /models），靠 base_url 末尾有没有 /v1 猜后缀一定会在其中一种协议上猜错。
-- 厂商行只放 base_url 与备注；完整路径按 (厂商, 协议) 存在 provider_routes 里，完整地址是
-- base_url 去掉末尾斜杠后直接拼 path。账号行仍固化自己的 base_url，改这里只影响之后新加的账号。
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 6。
CREATE TABLE providers (
  name     TEXT PRIMARY KEY,
  base_url TEXT NOT NULL,
  note     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE provider_routes (
  provider    TEXT NOT NULL,
  protocol    TEXT NOT NULL,
  path        TEXT NOT NULL,
  models_path TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (provider, protocol)
);

INSERT INTO providers (name, base_url, note) VALUES
  ('anthropic',   'https://api.anthropic.com',                        ''),
  ('openai',      'https://api.openai.com',                           ''),
  ('deepseek',    'https://api.deepseek.com',                         ''),
  ('moonshot',    'https://api.moonshot.cn',                          ''),
  ('kimi',        'https://api.moonshot.cn',                          ''),
  ('openrouter',  'https://openrouter.ai/api',                        ''),
  ('ollama',      'http://127.0.0.1:11434',                           ''),
  ('commandcode', 'https://api.commandcode.ai/provider/v1',           '');

-- 路径照抄各家文档的写法。models_path 为空表示这家在这个协议下没有模型列表接口，
-- 同步时回退到同厂商里按协议名排序的第一条非空路径。
INSERT INTO provider_routes (provider, protocol, path, models_path) VALUES
  ('anthropic',   'anthropic',   '/v1/messages',           '/v1/models'),
  ('openai',      'openai-chat', '/v1/chat/completions',   '/v1/models'),
  ('openai',      'openai-responses', '/v1/responses',     '/v1/models'),
  ('deepseek',    'openai-chat', '/v1/chat/completions',   '/v1/models'),
  ('deepseek',    'anthropic',   '/anthropic/v1/messages', ''),
  ('moonshot',    'openai-chat', '/v1/chat/completions',   '/v1/models'),
  ('kimi',        'openai-chat', '/v1/chat/completions',   '/v1/models'),
  ('openrouter',  'openai-chat', '/v1/chat/completions',   '/v1/models'),
  ('ollama',      'openai-chat', '/v1/chat/completions',   '/v1/models'),
  ('commandcode', 'openai-chat', '/chat/completions',      '/models'),
  ('commandcode', 'anthropic',   '/messages',              '');
