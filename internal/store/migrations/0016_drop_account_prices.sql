-- 账号不再是价格的落点：单价属于「供应商 × 模型」。
--
-- 为什么账号上不该有价格：账号级价格是模型事实还没从账号里拆出去时的过渡形态。那时一个账号的
-- 四个价对它名下所有模型生效，于是同一个上游的同一个模型在不同账号下各有各的一份单价，改一次价
-- 要改 N 个账号，`usage` 也给同一件事算出 N 个不同金额。拆出 `provider_models`（0015）之后，
-- 厂商级标价存目录、套餐差异存 `account_models` 覆盖值，账号行上再留一份价只会成为第四条取值路径，
-- 让「这个模型多少钱」有两个答案。
--
-- 为什么搬成账号级覆盖值、而不是写成厂商级标价：账号上的价是个人套餐价，不是这家上游的公开标价。
-- 写成 `provider_models` 的 `price_*` 会把一个人的折扣价冒充成所有人适用的标价，污染同厂商的其它
-- 账号——它们本来该按目录价算（或者按各自的套餐价覆盖）。挂到 `account_models`（这个账号、这个模型）
-- 就只影响它自己，语义与旧行为一致：旧行为是「这个账号的价对它名下的每个模型生效」。
--
-- 三步在同一个迁移里：
--   一、把账号行的四个价搬进 `account_models` 的价格列：只补空的（该行那一列是 NULL 而账号上非 NULL
--       时写进去），已有的覆盖值一个字节不动。搬完的结果与行序、与重跑都无关。
--   二、`ALTER TABLE accounts DROP COLUMN` 删掉四个价格列。
--   三、本文件执行后 store.migrate 把 PRAGMA user_version 推到 16。
-- 中途失败整笔回滚，因此不会出现「列已经删了、价却还没搬过去」的库。
--
-- 没有 `account_models` 行的账号（从没同步或探活过）没有地方可挂：那几份价在升级时丢弃，这个账号的
-- 历史请求从此按「缺价」记 null，而不是记 0。搬不进去也不该凭空在目录里造一行模型事实。
--
-- 删列用 ALTER TABLE ... DROP COLUMN：本仓库依赖的 modernc.org/sqlite v1.61.0 内置 SQLite 3.54.1，
-- DROP COLUMN 自 3.35 起可用，所以不走「建新表 + 搬数据 + 重建索引 + 改引用」那一套。四个价格列
-- 既不在索引里，也没有外键、视图或触发器引用它们，删掉不需要重建索引。
UPDATE account_models
   SET price_in          = COALESCE(price_in,
         (SELECT a.price_in FROM accounts a WHERE a.id = account_models.account_id)),
       price_out         = COALESCE(price_out,
         (SELECT a.price_out FROM accounts a WHERE a.id = account_models.account_id)),
       price_cache_read  = COALESCE(price_cache_read,
         (SELECT a.price_cache_read FROM accounts a WHERE a.id = account_models.account_id)),
       price_cache_write = COALESCE(price_cache_write,
         (SELECT a.price_cache_write FROM accounts a WHERE a.id = account_models.account_id));

ALTER TABLE accounts DROP COLUMN price_in;

ALTER TABLE accounts DROP COLUMN price_out;

ALTER TABLE accounts DROP COLUMN price_cache_read;

ALTER TABLE accounts DROP COLUMN price_cache_write;
