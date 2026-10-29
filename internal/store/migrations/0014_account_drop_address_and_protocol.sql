-- 地址与默认协议只留在厂商行上（见 DESIGN 5.8）：账号只存 provider 名字。
--
-- 两步在同一个迁移里：先给「provider 不在 providers 表里」的账号补一行厂商，再把两列删掉。
-- 中途失败整笔回滚，因此不会出现「列已经删了、地址却还没地方存」的库。
-- 补建的厂商名取账号的 provider，地址取该账号的 base_url——同名账号有多个地址时取最小的那个，
-- 结果与行序、与重跑都无关；备注写明来源。默认协议取账号的 protocol，但只认四条线协议
-- （与 store.IsWireProtocol 同一份名单，迁移是静态 SQL，没法调 Go 函数），其它值一律留空，
-- 回到 ResolveDefaultProtocol 的固定顺序回退，而不是把脏值当成默认协议。
-- 已有厂商（含 0006、0007、0009 的种子）一个字段都不动：INSERT OR IGNORE 撞主键就跳过，
-- 账号上历史遗留的 protocol 因此不会覆盖用户已经设好的 default_protocol。
--
-- 删列用 ALTER TABLE ... DROP COLUMN：本仓库依赖的 modernc.org/sqlite v1.61.0 内置 SQLite 3.54.1，
-- DROP COLUMN 自 3.35 起可用，所以不走「建新表 + 搬数据 + 重建索引 + 改引用」那一套。
-- accounts 上只有 name 的唯一约束与 accounts_name 索引，两列既不在索引里，也没有外键、视图或
-- 触发器引用它们，删掉不需要重建索引。
--
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 14。
INSERT OR IGNORE INTO providers (name, base_url, note, default_protocol)
  SELECT a.provider,
         MIN(a.base_url),
         '由迁移 0014 从账号地址补建',
         COALESCE((SELECT b.protocol FROM accounts b
                    WHERE b.provider = a.provider
                      AND b.protocol IN ('anthropic', 'openai-chat', 'openai-responses', 'gemini')
                    ORDER BY b.name LIMIT 1), '')
    FROM accounts a
   WHERE a.provider <> ''
   GROUP BY a.provider;

ALTER TABLE accounts DROP COLUMN base_url;

ALTER TABLE accounts DROP COLUMN protocol;
