// 语音方言的 hook 实现：请求由 JS 拼，响应的 base64 音频由 JS 解。
// 这个文件用真实的沙箱跑一遍，钉住「沙箱 → 方言 → 网关」这条链路。

export async function prepare_request(ctx) {
  return {
    body: {
      kind: "json",
      value: {
        model: ctx.model,
        messages: [{role: "assistant", content: ctx.input.text}],
        audio: {format: ctx.input.format},
      },
    },
  };
}

export async function decode_response(ctx) {
  const choice = ctx.response.body.choices[0];
  return {
    result: {kind: "media", mime: "audio/mpeg", body_base64: choice.message.audio.data},
    usage: {input_tokens: 3, output_tokens: 1, cached_input_tokens: 2, source: "upstream"},
  };
}
