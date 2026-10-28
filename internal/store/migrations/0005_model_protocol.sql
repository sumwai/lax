-- 协议降到模型上：同一个 base_url、同一个 key，claude-* 只认 /messages，其余模型走
-- /v1/chat/completions。空字符串表示继承账号的 protocol，老库升上来时全部是空串，
-- 也就是行为与升级前一致：按账号协议打。
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 5。
ALTER TABLE account_models ADD COLUMN protocol TEXT NOT NULL DEFAULT '';
