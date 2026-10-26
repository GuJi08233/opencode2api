// One request per process keeps cancellation and response ownership with Go.
// Request data travels over stdin, never in command-line arguments or logs.
let sentHeaders = false;
let headerTimer;
try {
  const input = await Bun.stdin.json();
  const controller = new AbortController();
  if (input.headerTimeoutMs > 0) {
    headerTimer = setTimeout(() => controller.abort(), input.headerTimeoutMs);
  }
  const response = await fetch(input.url, {
    method: input.method,
    headers: input.headers,
    body: input.body === null ? undefined : Buffer.from(input.body, "base64"),
    signal: controller.signal,
    redirect: "manual",
    // A configured direct route must not inherit the host's proxy environment.
    proxy: input.proxy ? { url: input.proxy, respectNoProxy: false } : false,
  });
  clearTimeout(headerTimer);
  const headers = [];
  const decoded = /^(gzip|x-gzip|deflate|br|zstd)$/i.test(
    response.headers.get("content-encoding") || "",
  );
  for (const [name, value] of response.headers) {
    // fetch decodes the body. Framing and lengths cannot describe our pipe.
    if (["content-length", "transfer-encoding", "connection"].includes(name)) continue;
    if (name === "content-encoding" && decoded) continue;
    if (name === "set-cookie" && typeof response.headers.getSetCookie === "function") continue;
    headers.push([name, value]);
  }
  if (typeof response.headers.getSetCookie === "function") {
    for (const value of response.headers.getSetCookie()) headers.push(["set-cookie", value]);
  }
  await Bun.stdout.write(JSON.stringify({
    status: response.status,
    statusText: response.statusText,
    headers,
    decoded,
  }) + "\n");
  sentHeaders = true;
  if (response.body) {
    const reader = response.body.getReader();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      await Bun.stdout.write(value);
    }
  }
  process.exit(0);
} catch (error) {
  clearTimeout(headerTimer);
  // Upstream URLs and response bodies can contain credentials. Only a stable
  // error category crosses the process boundary; no raw exception is logged.
  if (!sentHeaders) {
    const timeout = error?.name === "AbortError" || error?.name === "TimeoutError";
    await Bun.stdout.write(JSON.stringify({ error: timeout ? "timeout" : "network" }) + "\n");
  }
  process.exit(1);
}
