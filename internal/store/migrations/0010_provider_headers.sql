-- 厂商附加请求头：有些上游除标准凭据头之外还要求别的头才能路由（opencode 的 Go 端点要
-- `x-opencode-session`，Azure 一类要 `api-version`），这些头的名字与值由厂商约定，不是协议差异，
-- 因此不为它们写代码分支，改成按 (厂商, 协议, 头名) 存一行声明，转发时按声明加上去。
--
-- protocol 为空串表示整个厂商的每条路由都加，非空表示只加给这条协议；同一头名两边都有时协议级
-- 覆盖厂商级（解析在 store.EffectiveProviderHeaders，表里两行都留着，删协议级那行就回退）。
-- name 落库前由 store 归一成小写（HTTP 头名大小写不敏感），value 原样存，校验在 store 的写入口。
--
-- provider 外键指向 providers(name)（那是这张表的主键），因此删厂商行时它的头一起走：
-- store.Open 的连接串带 _pragma=foreign_keys(1)，级联是真的生效的，不需要像 provider_routes
-- 那样在 DeleteProvider 里手删（那张表建于 0006，当时没有外键）。
-- 主键 (provider, protocol, name) 的最左列就是 provider，级联删除与按厂商读都走这个索引。
--
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 10。
CREATE TABLE provider_headers (
  provider TEXT NOT NULL REFERENCES providers(name) ON DELETE CASCADE,
  protocol TEXT NOT NULL,
  name     TEXT NOT NULL,
  value    TEXT NOT NULL,
  PRIMARY KEY (provider, protocol, name)
);
