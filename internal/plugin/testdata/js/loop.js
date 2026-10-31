// text 是 loop 时死循环，用来验证中断，以及超时之后下一次调用仍然可用。
export function prepare_request(ctx) {
  if (ctx.input.text === "loop") {
    for (;;) {}
  }
  return {body: {kind: "json", value: {ok: true}}};
}
