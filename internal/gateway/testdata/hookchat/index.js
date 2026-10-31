// 对话操作的 hook 方言：在宿主编好的 chat 报文上加字段，并把响应的正文改写过一次，
// 证明「对话请求也能由插件打补丁、由插件解响应」。

export async function prepare_request(ctx) {
  const body = {};
  for (const key in ctx.body) {
    body[key] = ctx.body[key];
  }
  body.enable_thinking = true;
  return {body: body};
}

export async function decode_response(ctx) {
  const resp = {};
  for (const key in ctx.response.body) {
    resp[key] = ctx.response.body[key];
  }
  resp.choices[0].message.content = "钩子:" + resp.choices[0].message.content;
  return {
    result: {kind: "json", json: resp},
    usage: {input_tokens: 5, output_tokens: 2, source: "upstream"},
  };
}
