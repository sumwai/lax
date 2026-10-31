-- 思考 token 单独记两列。
--
-- reasoning_tokens 是上游上报的思考 token 原值：chat 读 completion_tokens_details.reasoning_tokens，
-- responses 读 output_tokens_details.reasoning_tokens，gemini 读 thoughtsTokenCount；
-- anthropic 没有独立字段（output_tokens 本来就含思考），记 0。
--
-- reasoning_tokens_extra 是其中「输出之外的一项」那部分，只给这一列按输出价再计一次。
-- 判定按上报的数字做（store.ReasoningExtra）：有的中转只把思考报在这个字段里、没有并进
-- completion_tokens，total 却是 prompt + completion + reasoning；OpenAI 官方则把 reasoning
-- 当作 completion_tokens 的子集。两种都直接相加会把官方那条重复收一遍钱，一律不单独算则漏掉
-- 中转那九成以上的输出花费，所以只把「total > prompt + completion」的那部分记进 extra。
--
-- 历史行没有这个数，两列都留默认值 0：当时上游报了什么已经无从复原，写 0 让历史花费
-- 与升级前完全一致（不凭空变多，也不假装当时没有思考）。
--
-- 本文件执行后 store.migrate 把 PRAGMA user_version 推到 12。
ALTER TABLE requests ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE requests ADD COLUMN reasoning_tokens_extra INTEGER NOT NULL DEFAULT 0;
