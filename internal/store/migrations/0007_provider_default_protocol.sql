-- 默认协议进库：providers 一行现在可以显式声明默认协议，`account add` 没给 --protocol 时用它，
-- `provider ls` 也展示它。空字符串表示这家没有显式默认，调用方回退到按协议名排序的第一条路由；
-- 用户自建的厂商可以不填。
-- 为什么不能一律靠排序猜：deepseek 与 commandcode 都各声明了 openai-chat 与 anthropic 两条路由，
-- 协议名排序把 anthropic 排在前面，默认协议会从升级前的 openai-chat 变成 anthropic，
-- 这两个厂商的账号在升级前后默认协议不一致。所以升级前的默认值在这里显式种下来。
-- 只给已有的厂商行赋默认协议：用户删掉的厂商不会被重新建出来，自建厂商保持空串。
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 7。
ALTER TABLE providers ADD COLUMN default_protocol TEXT NOT NULL DEFAULT '';

UPDATE providers SET default_protocol = 'anthropic' WHERE name = 'anthropic';

UPDATE providers SET default_protocol = 'openai-chat'
 WHERE name IN ('openai', 'deepseek', 'moonshot', 'kimi', 'openrouter', 'ollama', 'commandcode');
