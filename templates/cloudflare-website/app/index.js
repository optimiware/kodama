export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/healthz") {
      return Response.json({ status: "ok", service: "website" });
    }
    return new Response("Hello from the Kodama Cloudflare example.\n", {
      headers: { "content-type": "text/plain; charset=utf-8" },
    });
  },
};
