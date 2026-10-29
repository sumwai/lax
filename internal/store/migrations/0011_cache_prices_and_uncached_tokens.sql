-- 缓存读写按单价计入估算花费，并把「未命中缓存的输入 token」单独记一列（见 DESIGN 5.5、6）。
--
-- price_cache_read 与 price_cache_write 是账号上的缓存单价（每百万 token），可空：
-- 空表示没填，花费估算按输入价 price_in 回退（SQL 里是 COALESCE(price_cache_read, price_in, 0)）。
-- 老账号两列都是 NULL，也就是回退到输入价，行为与只填输入输出价时一致。
--
-- in_tokens_uncached 是入账时按出站协议归一化之后的输入 token：anthropic 的 input_tokens
-- 已排除缓存读写，直接用；chat 的 prompt_tokens 与 gemini 的 promptTokenCount 含命中缓存的部分，
-- 要减掉 cache_read（减到 0 为止）。in_tokens 仍是从上游读到的原值，展示不变，只有花费用新列。
--
-- 历史行没有区分能力，按当时的 in_tokens 回填：那是当时唯一知道的输入数，按原值最接近。
-- 这比一律写 0 诚实：写 0 会让历史花费凭空变成只算输出。
--
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 11。
ALTER TABLE accounts ADD COLUMN price_cache_read REAL;
ALTER TABLE accounts ADD COLUMN price_cache_write REAL;
ALTER TABLE requests ADD COLUMN in_tokens_uncached INTEGER NOT NULL DEFAULT 0;
UPDATE requests SET in_tokens_uncached = in_tokens;
