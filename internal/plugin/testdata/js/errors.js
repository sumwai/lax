export function prepare_request(ctx) {
  const mode = ctx.input.text;
  if (mode === "lax") {
    throw lax.error("rate_limited", "上游限流", {retryable: true, details: "acct=7"});
  }
  if (mode === "unknown") {
    throw lax.error("no_such_code", "未知的错误码");
  }
  if (mode === "plain") {
    throw new Error("普通错误");
  }
  if (mode === "shape") {
    return "不是对象";
  }
  if (mode === "unknownfield") {
    return {body: {kind: "json", value: {}}, extra: 1};
  }
  return {body: {kind: "json", value: {ok: true}}};
}
