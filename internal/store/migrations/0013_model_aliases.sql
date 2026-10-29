-- 模型别名表（见 DESIGN 5.12、6）。
--
-- 客户端的模型名常常写死（claude-3-5-sonnet、gpt-4o），而账号声明的是上游的真实名字
-- （glm-4.6、gemini-3.8-flash）。这张表让客户端继续用自己认识的名字：alias 是客户端给的
-- 名字，target 是真实模型名或另一个别名，解析沿链走（最长 8 跳，成环报错，见
-- store.ResolveModelAlias），客户端不必改，也不必给账号编假模型名。
--
-- 全局一张表，不按账号分：按账号分会让同一个别名在不同厂商下指向不同模型，客户端拿到的
-- 名字就不再稳定；要按厂商区分就写两个别名（fast-openai、fast-glm）。
--
-- 只加表，0001–0012 一个字不动。老库升上来这张表是空的，所有名字照旧按真实名字匹配，
-- 升级前后行为完全一致。
--
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 13。
CREATE TABLE model_aliases (
  alias      TEXT    PRIMARY KEY,
  target     TEXT    NOT NULL,
  created_at INTEGER NOT NULL
);
