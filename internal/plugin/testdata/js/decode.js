// 响应侧：文本、音频与已经包好的 JSON 各一种。
export function decode_response(ctx) {
  const response = ctx.response;
  if (response.content_type.indexOf("json") >= 0) {
    return {
      result: {kind: "json", json: {text: response.body.text, mouth: "closed"}},
      usage: {
        input_tokens: 3,
        output_tokens: 5,
        cached_input_tokens: 1,
        total_tokens: 8,
        source: "upstream",
      },
      finish_reason: "stop",
    };
  }
  if (response.content_type.indexOf("audio") >= 0) {
    return {result: {kind: "media", mime: response.content_type, body_base64: response.media_base64}};
  }
  return {result: {kind: "text", text: response.text}};
}
