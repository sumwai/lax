-- 上游失败处理策略：条件与动作都是显式列，匹配顺序按 priority、id。
-- account_id 为空表示不限账号；非空时挂在外键上，账号删除即级联清掉。
-- statuses 与 error_contains 是 JSON 数组文本，空串或 [] 表示任意。
--
-- 本文件只给测试按版本号执行（openDatabaseAt）；运行时的新库由 AutoMigrate 按
-- model.go 里的 routeFailurePolicyRow 建出同一形状，store.migrate 不执行本文件，也不维护 user_version。

CREATE TABLE route_failure_policies (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  enabled          INTEGER NOT NULL DEFAULT 1,
  priority         INTEGER NOT NULL DEFAULT 100,
  account_id       INTEGER REFERENCES accounts(id) ON DELETE CASCADE,
  provider         TEXT    NOT NULL DEFAULT '',
  model            TEXT    NOT NULL DEFAULT '',
  statuses         TEXT    NOT NULL DEFAULT '',
  error_contains   TEXT    NOT NULL DEFAULT '',
  error_regex      TEXT    NOT NULL DEFAULT '',
  retry_count      INTEGER NOT NULL DEFAULT 0,
  retry_base_ms    INTEGER NOT NULL DEFAULT 500,
  cooldown_seconds INTEGER NOT NULL DEFAULT 0,
  disable          INTEGER NOT NULL DEFAULT 0,
  next_route       INTEGER NOT NULL DEFAULT 0,
  return_error     INTEGER NOT NULL DEFAULT 0,
  note             TEXT    NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL DEFAULT 0,
  updated_at       INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX route_failure_policies_order ON route_failure_policies (enabled, priority, id);

-- 账号级冷却（Unix 毫秒）。旧的 accounts.cooldown_until（秒）只是只读汇总，不要复用它；
-- 按模型隔离的冷却仍在 account_models.cooldown_until_ms。
ALTER TABLE accounts ADD COLUMN cooldown_until_ms INTEGER;
