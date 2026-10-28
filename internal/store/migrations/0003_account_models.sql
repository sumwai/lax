CREATE TABLE account_models (
  account_id  INTEGER NOT NULL,
  model       TEXT    NOT NULL,
  status      TEXT    NOT NULL,
  http_status INTEGER NOT NULL DEFAULT 0,
  error       TEXT    NOT NULL DEFAULT '',
  checked_at  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, model)
);
