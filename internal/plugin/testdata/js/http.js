export async function prepare_request(ctx) {
  const options = {method: ctx.options.method || "GET", url: ctx.options.url};
  if (ctx.options.body) {
    options.body = ctx.options.body;
  }
  const response = await lax.http.request(options);
  return {body: {kind: "json", value: {status: response.status, body: response.body}}};
}
