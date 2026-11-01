// 请求头的返回形状：插件原样给对象，键名归一、值校验、保留头拒绝都由宿主做。
export async function prepare_request(ctx) {
  const raw = ctx.options.headers || "{}";
  return {headers: JSON.parse(raw), body: {kind: "json", value: {ok: true}}};
}
