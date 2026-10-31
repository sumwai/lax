export function prepare_request(ctx) {
  lax.log("warn", ctx.input.text);
  console.error("console", "多参数");
  return {body: {kind: "json", value: {ok: true}}};
}
