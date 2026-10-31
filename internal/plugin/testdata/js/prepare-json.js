// 第一种返回形状：body 是 {kind: "json", value: {…}}。
export async function prepare_request(ctx) {
  return {
    method: "POST",
    path: "/v1/speech",
    headers: {"x-plugin": "test"},
    body: {kind: "json", value: {model: ctx.model, text: ctx.input.text}},
  };
}
