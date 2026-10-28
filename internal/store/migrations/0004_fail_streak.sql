-- 冷却按连续失败次数翻倍，需要把次数记在账号上，跨请求累计。
ALTER TABLE accounts ADD COLUMN fail_streak INTEGER NOT NULL DEFAULT 0;
