-- 模型阶梯与分时计费规则（JSON 文本）以及 requests 表的请求花费快照。
--
-- 本文件只给测试按版本号执行（openDatabaseAt）；运行时的新库由 AutoMigrate 按
-- model.go 里的 row 定义建出同一形状，store.migrate 不执行本文件，也不维护 user_version。

ALTER TABLE requests ADD COLUMN cost REAL;
ALTER TABLE provider_models ADD COLUMN pricing_rule TEXT;
ALTER TABLE account_models ADD COLUMN pricing_rule TEXT;
