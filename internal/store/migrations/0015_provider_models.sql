-- 模型事实与账号可达性拆成两张表：provider_models 存上游的模型事实，
-- 同一个模型在多家账号下只存一份；account_models 只存「这个账号能不能用这个模型」与
-- 账号侧的覆盖值（协议、长度、价格），同一个模型在三个账号下不会有三份重复元数据。
--
-- 三步在同一个迁移里：先建目录表，再把 account_models 改成新形状并把 status 映射成 access，
-- 最后把 accounts.models 里手填的名字补进两张表并删掉那一列。中途失败整笔回滚，因此不会出现
-- 「列已经删了、名字却还没落进可达性表」的库。
--
-- 映射：usable → allowed，forbidden → forbidden，notfound → missing，其余 → error。
-- synced（同步过、从未探活）与 auth（凭据被拒）都落成 error：它们在旧库里本来就不是选号依据，
-- accounts.models 里的名字由下面第四步单独补成 allowed，选号结果因此与升级前一致。
--
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 15。

CREATE TABLE provider_models (
  provider        TEXT    NOT NULL,
  model           TEXT    NOT NULL,
  source          TEXT    NOT NULL,           -- sync / manual / probe
  context_window  INTEGER,
  max_output      INTEGER,
  protocols       TEXT    NOT NULL DEFAULT '', -- 该模型支持的协议，逗号分隔
  capabilities    TEXT    NOT NULL DEFAULT '',
  price_in        REAL,
  price_out       REAL,
  price_cache_read  REAL,
  price_cache_write REAL,
  updated_at      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (provider, model)
);

ALTER TABLE account_models RENAME TO account_models_old;

CREATE TABLE account_models (
  account_id      INTEGER NOT NULL,
  model           TEXT    NOT NULL,
  access          TEXT    NOT NULL,           -- allowed / forbidden / missing / error
  http_status     INTEGER NOT NULL DEFAULT 0,
  error           TEXT    NOT NULL DEFAULT '',
  checked_at      INTEGER NOT NULL DEFAULT 0,
  protocol        TEXT    NOT NULL DEFAULT '', -- 账号级的协议覆盖（探活发现）
  context_window  INTEGER,                     -- NULL 表示继承目录
  max_output      INTEGER,
  price_in        REAL,
  price_out       REAL,
  price_cache_read  REAL,
  price_cache_write REAL,
  PRIMARY KEY (account_id, model)
);

INSERT INTO account_models (account_id, model, access, http_status, error, checked_at, protocol)
  SELECT account_id, model,
         CASE status
           WHEN 'usable'    THEN 'allowed'
           WHEN 'forbidden' THEN 'forbidden'
           WHEN 'notfound'  THEN 'missing'
           ELSE 'error'
         END,
         http_status, error, checked_at, protocol
    FROM account_models_old
   ORDER BY account_id, model;

DROP TABLE account_models_old;

-- 每个账号的模型行搬进目录：厂商取账号的 provider，来源记 probe（这些行都是探活或同步留下的
-- 结论，不是用户手填的）。旧行的协议是「这个模型在这个账号上该用哪种协议」，正是目录里的
-- protocols 列能放下的东西，其余列（长度、能力位、价格、updated_at）旧行没有对应来源，留空。
INSERT INTO provider_models (provider, model, source, protocols, updated_at)
  SELECT a.provider, m.model, 'probe', m.protocol, 0
    FROM account_models m
    JOIN accounts a ON a.id = m.account_id
   WHERE a.provider <> ''
   ORDER BY a.provider, m.model, m.protocol
  ON CONFLICT (provider, model) DO NOTHING;

-- 同一厂商的多个账号各探到不同的协议时，上面那句的插入顺序决定谁先落行，不保证留下非空的那个。
-- 这里补一次：目录里协议还是空串、而某个账号探到过协议时，取字典序最小的那个，结果与行序无关。
UPDATE provider_models
   SET protocols = (
         SELECT MIN(m.protocol) FROM account_models m
           JOIN accounts a ON a.id = m.account_id
          WHERE a.provider = provider_models.provider
            AND m.model = provider_models.model
            AND m.protocol <> '')
 WHERE protocols = ''
   AND EXISTS (SELECT 1 FROM account_models m
                 JOIN accounts a ON a.id = m.account_id
                WHERE a.provider = provider_models.provider
                  AND m.model = provider_models.model
                  AND m.protocol <> '');

-- accounts.models 是手填的名字，等价于「该账号 access='allowed' 的行」。里面出现但 account_models
-- 没有的名字补两份：目录里补一行 manual（已有的行不动，探活留下的行不因为手填而改来源），
-- 可达性表里补一行 allowed。json_valid 挡住历史脏值：一条坏 JSON 不该让整次升级失败。
INSERT INTO provider_models (provider, model, source, updated_at)
  SELECT DISTINCT a.provider, j.value, 'manual', 0
    FROM accounts a, json_each(a.models) j
   WHERE json_valid(a.models)
     AND a.provider <> ''
     AND TRIM(j.value) <> ''
     AND NOT EXISTS (SELECT 1 FROM provider_models pm
                      WHERE pm.provider = a.provider AND pm.model = j.value)
   ORDER BY 1, 2;

INSERT INTO account_models (account_id, model, access, http_status, error, checked_at, protocol)
  SELECT DISTINCT a.id, j.value, 'allowed', 0, '', 0, ''
    FROM accounts a, json_each(a.models) j
   WHERE json_valid(a.models)
     AND TRIM(j.value) <> ''
  ON CONFLICT (account_id, model) DO NOTHING;

ALTER TABLE accounts DROP COLUMN models;
