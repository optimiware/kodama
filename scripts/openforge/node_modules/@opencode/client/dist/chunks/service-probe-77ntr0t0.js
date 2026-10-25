import {
  ClientError3
} from "./service-probe-nwcpakzx.js";
import {
  __export
} from "./service-probe-9rqn6x4v.js";

// src/promise/generated/client.ts
var exports_client4 = {};
__export(exports_client4, {
  make: () => make6
});
var maxSseEventBytes = 16 * 1024 * 1024;
function make6(options) {
  const fetch = options.fetch ?? globalThis.fetch;
  const prepare = (descriptor, requestOptions) => {
    const baseUrl = new URL(options.baseUrl);
    if (!baseUrl.pathname.endsWith("/"))
      baseUrl.pathname += "/";
    const url = new URL(descriptor.path.slice(1), baseUrl);
    for (const [key, value] of Object.entries(descriptor.query ?? {}))
      appendQuery(url.searchParams, key, value);
    const headers = new Headers(options.headers);
    for (const [key, value] of Object.entries(descriptor.headers ?? {})) {
      if (value !== undefined && value !== null)
        headers.set(key, String(value));
    }
    for (const [key, value] of new Headers(requestOptions?.headers))
      headers.set(key, value);
    if (descriptor.body !== undefined && !headers.has("content-type"))
      headers.set("content-type", descriptor.binaryBody ? "application/octet-stream" : "application/json");
    return {
      url,
      init: {
        method: descriptor.method,
        signal: requestOptions?.signal,
        headers,
        body: descriptor.body === undefined ? undefined : descriptor.binaryBody ? descriptor.body : JSON.stringify(descriptor.body)
      }
    };
  };
  const execute = async (descriptor, requestOptions) => {
    try {
      const prepared = prepare(descriptor, requestOptions);
      return await fetch(prepared.url, prepared.init);
    } catch (cause) {
      throw new ClientError3("Transport", { cause });
    }
  };
  const responseError = async (response, descriptor) => {
    if (descriptor.declaredStatuses.includes(response.status))
      throw declared(await json(response));
    try {
      await response.body?.cancel();
    } catch {}
    throw new ClientError3("UnexpectedStatus", { cause: { status: response.status }, detail: String(response.status) });
  };
  const request = async (descriptor, requestOptions) => {
    const response = await execute(descriptor, requestOptions);
    if (response.status !== descriptor.successStatus)
      return responseError(response, descriptor);
    if (descriptor.binary)
      return new Uint8Array(await response.arrayBuffer());
    if (descriptor.empty) {
      try {
        await response.body?.cancel();
      } catch {}
      return;
    }
    return await json(response);
  };
  const sse = (descriptor, requestOptions) => ({
    async* [Symbol.asyncIterator]() {
      const response = await execute(descriptor, requestOptions);
      if (response.status !== descriptor.successStatus)
        await responseError(response, descriptor);
      if (!isContentType(response, "text/event-stream")) {
        try {
          await response.body?.cancel();
        } catch {}
        throw new ClientError3("UnsupportedContentType", { detail: response.headers.get("content-type") });
      }
      if (response.body === null)
        throw new ClientError3("MalformedResponse");
      const reader = response.body.getReader();
      const decoder = new TextDecoder;
      let buffer = "";
      try {
        while (true) {
          let next;
          try {
            next = await reader.read();
          } catch (cause) {
            throw new ClientError3("Transport", { cause });
          }
          if (!next.done)
            requestOptions?.onActivity?.();
          buffer += decoder.decode(next.value, { stream: !next.done });
          if (buffer.length > maxSseEventBytes)
            throw new ClientError3("SseEventTooLarge");
          const trailingCarriageReturn = !next.done && buffer.endsWith("\r");
          if (trailingCarriageReturn)
            buffer = buffer.slice(0, -1);
          buffer = buffer.replaceAll(`\r
`, `
`).replaceAll("\r", `
`);
          if (trailingCarriageReturn)
            buffer += "\r";
          if (next.done && buffer !== "")
            buffer += `

`;
          let boundary = buffer.indexOf(`

`);
          while (boundary >= 0) {
            const block = buffer.slice(0, boundary);
            buffer = buffer.slice(boundary + 2);
            const data = block.split(`
`).flatMap((line) => line.startsWith("data:") ? [line.slice(5).trimStart()] : []).join(`
`);
            if (data !== "") {
              try {
                yield JSON.parse(data);
              } catch (cause) {
                throw new ClientError3("MalformedResponse", { cause });
              }
            }
            boundary = buffer.indexOf(`

`);
          }
          if (next.done)
            return;
        }
      } finally {
        try {
          await reader.cancel();
        } catch {}
        reader.releaseLock();
      }
    }
  });
  return {
    server: {
      info: (requestOptions) => request({ method: "GET", path: `/api/info`, successStatus: 200, declaredStatuses: [400, 401], empty: false }, requestOptions),
      pair: (requestOptions) => request({ method: "POST", path: `/api/pair`, successStatus: 200, declaredStatuses: [400, 401], empty: false }, requestOptions),
      connect: (input, requestOptions) => request({
        method: "GET",
        path: `/auth/connect/${encodeURIComponent(input.code)}`,
        successStatus: 200,
        declaredStatuses: [400, 401],
        empty: false
      }, requestOptions)
    },
    location: {
      get: (input, requestOptions) => request({
        method: "GET",
        path: `/api/location`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      reload: (requestOptions) => request({
        method: "POST",
        path: `/api/location/reload`,
        successStatus: 204,
        declaredStatuses: [400, 401, 503],
        empty: true
      }, requestOptions)
    },
    agent: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/agent`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      get: (input, requestOptions) => request({
        method: "GET",
        path: `/api/agent/${encodeURIComponent(input.agentID)}`,
        query: { location: input["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions)
    },
    plugin: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/plugin`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      check: (input, requestOptions) => request({
        method: "POST",
        path: `/api/plugin/check`,
        query: { location: input?.["location"] },
        body: { target: input?.["target"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      update: (input, requestOptions) => request({
        method: "POST",
        path: `/api/plugin/update`,
        query: { location: input["location"] },
        body: { targets: input["targets"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404, 503],
        empty: true
      }, requestOptions)
    },
    session: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/session`,
        query: {
          limit: input?.["limit"],
          order: input?.["order"],
          search: input?.["search"],
          parentID: input?.["parentID"],
          archived: input?.["archived"],
          directory: input?.["directory"],
          project: input?.["project"],
          subpath: input?.["subpath"],
          cursor: input?.["cursor"]
        },
        successStatus: 200,
        declaredStatuses: [400, 401],
        empty: false
      }, requestOptions),
      stats: (input, requestOptions) => request({
        method: "GET",
        path: `/api/experimental/session/stats`,
        query: {
          from: input?.["from"],
          to: input?.["to"],
          project: input?.["project"],
          timezone: input?.["timezone"],
          tools: input?.["tools"]
        },
        successStatus: 200,
        declaredStatuses: [400, 401],
        empty: false
      }, requestOptions).then((value) => value.data),
      create: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session`,
        body: {
          id: input?.["id"],
          parentID: input?.["parentID"],
          title: input?.["title"],
          agent: input?.["agent"],
          model: input?.["model"],
          location: input?.["location"],
          metadata: input?.["metadata"],
          permissions: input?.["permissions"]
        },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions).then((value) => value.data),
      import: (input, requestOptions) => request({
        method: "POST",
        path: `/api/experimental/session/import`,
        body: { info: input["info"], messages: input["messages"], location: input["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 409],
        empty: false
      }, requestOptions).then((value) => value.data),
      export: (input, requestOptions) => request({
        method: "GET",
        path: `/api/experimental/session/${encodeURIComponent(input.sessionID)}/export`,
        query: { sanitize: input["sanitize"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 500],
        empty: false
      }, requestOptions).then((value) => value.data),
      active: (requestOptions) => request({
        method: "GET",
        path: `/api/session/active`,
        successStatus: 200,
        declaredStatuses: [400, 401],
        empty: false
      }, requestOptions).then((value) => value.data),
      get: (input, requestOptions) => request({
        method: "GET",
        path: `/api/session/${encodeURIComponent(input.sessionID)}`,
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions).then((value) => value.data),
      remove: (input, requestOptions) => request({
        method: "DELETE",
        path: `/api/session/${encodeURIComponent(input.sessionID)}`,
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      fork: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/fork`,
        body: { before: input["before"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions).then((value) => value.data),
      switchAgent: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/agent`,
        body: { agent: input["agent"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      switchModel: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/model`,
        body: { model: input["model"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      update: (input, requestOptions) => request({
        method: "PATCH",
        path: `/api/session/${encodeURIComponent(input.sessionID)}`,
        body: {
          title: input["title"],
          metadata: input["metadata"],
          permissions: input["permissions"],
          archived: input["archived"]
        },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      move: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/move`,
        body: { directory: input["directory"], delivery: input["delivery"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      prompt: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/prompt`,
        body: {
          id: input["id"],
          text: input["text"],
          files: input["files"],
          agents: input["agents"],
          skills: input["skills"],
          metadata: input["metadata"],
          delivery: input["delivery"],
          resume: input["resume"]
        },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 409],
        empty: false
      }, requestOptions).then((value) => value.data),
      command: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/command`,
        body: {
          name: input["name"],
          text: input["text"],
          files: input["files"],
          agents: input["agents"],
          skills: input["skills"],
          delivery: input["delivery"]
        },
        successStatus: 204,
        declaredStatuses: [400, 401, 404, 500],
        empty: true
      }, requestOptions),
      skill: (input, requestOptions) => request({
        method: "POST",
        path: `/api/experimental/session/${encodeURIComponent(input.sessionID)}/skill`,
        body: { id: input["id"], resume: input["resume"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      synthetic: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/synthetic`,
        body: {
          id: input["id"],
          text: input["text"],
          description: input["description"],
          metadata: input["metadata"],
          delivery: input["delivery"],
          resume: input["resume"]
        },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 409],
        empty: false
      }, requestOptions).then((value) => value.data),
      shell: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/shell`,
        body: { id: input["id"], command: input["command"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      compact: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/compact`,
        body: { id: input["id"], delivery: input["delivery"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 409],
        empty: false
      }, requestOptions).then((value) => value.data),
      wait: (input, requestOptions) => request({
        method: "POST",
        path: `/api/experimental/session/${encodeURIComponent(input.sessionID)}/wait`,
        successStatus: 204,
        declaredStatuses: [400, 401, 404, 503],
        empty: true
      }, requestOptions),
      revert: {
        stage: (input, requestOptions) => request({
          method: "POST",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/revert/stage`,
          body: { messageID: input["messageID"], files: input["files"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404, 409, 500],
          empty: false
        }, requestOptions).then((value) => value.data),
        clear: (input, requestOptions) => request({
          method: "DELETE",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/revert`,
          successStatus: 204,
          declaredStatuses: [400, 401, 404, 409, 500],
          empty: true
        }, requestOptions),
        commit: (input, requestOptions) => request({
          method: "POST",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/revert/commit`,
          successStatus: 204,
          declaredStatuses: [400, 401, 404, 409],
          empty: true
        }, requestOptions)
      },
      context: (input, requestOptions) => request({
        method: "GET",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/context`,
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 500],
        empty: false
      }, requestOptions).then((value) => value.data),
      diff: (input, requestOptions) => request({
        method: "GET",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/diff`,
        query: { from: input["from"], to: input["to"], context: input["context"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 500],
        empty: false
      }, requestOptions).then((value) => value.data),
      inbox: {
        list: (input, requestOptions) => request({
          method: "GET",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/inbox`,
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions).then((value) => value.data),
        cancel: (input, requestOptions) => request({
          method: "DELETE",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/inbox/${encodeURIComponent(input.inboxID)}`,
          successStatus: 204,
          declaredStatuses: [400, 401, 404],
          empty: true
        }, requestOptions),
        update: (input, requestOptions) => request({
          method: "PATCH",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/inbox/${encodeURIComponent(input.inboxID)}`,
          body: { delivery: input["delivery"] },
          successStatus: 204,
          declaredStatuses: [400, 401, 404, 409],
          empty: true
        }, requestOptions)
      },
      instructions: {
        entry: {
          list: (input, requestOptions) => request({
            method: "GET",
            path: `/api/experimental/session/${encodeURIComponent(input.sessionID)}/instructions/entries`,
            successStatus: 200,
            declaredStatuses: [400, 401, 404],
            empty: false
          }, requestOptions).then((value) => value.data),
          put: (input, requestOptions) => request({
            method: "PUT",
            path: `/api/experimental/session/${encodeURIComponent(input.sessionID)}/instructions/entries/${encodeURIComponent(input.key)}`,
            body: { value: input["value"] },
            successStatus: 204,
            declaredStatuses: [400, 401, 404, 413],
            empty: true
          }, requestOptions),
          remove: (input, requestOptions) => request({
            method: "DELETE",
            path: `/api/experimental/session/${encodeURIComponent(input.sessionID)}/instructions/entries/${encodeURIComponent(input.key)}`,
            successStatus: 204,
            declaredStatuses: [400, 401, 404],
            empty: true
          }, requestOptions)
        }
      },
      generate: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/generate`,
        body: { prompt: input["prompt"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 503],
        empty: false
      }, requestOptions).then((value) => value.data),
      log: (input, requestOptions) => sse({
        method: "GET",
        path: `/api/experimental/session/${encodeURIComponent(input.sessionID)}/log`,
        query: { after: input["after"], follow: input["follow"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      interrupt: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/interrupt`,
        query: { resume: input["resume"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      background: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/background`,
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      message: {
        get: (input, requestOptions) => request({
          method: "GET",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/message/${encodeURIComponent(input.messageID)}`,
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions).then((value) => value.data)
      },
      form: {
        list: (input, requestOptions) => request({
          method: "GET",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/form`,
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions).then((value) => value.data),
        create: (input, requestOptions) => request({
          method: "POST",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/form`,
          body: { id: input["id"], title: input["title"], metadata: input["metadata"], fields: input["fields"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404, 409],
          empty: false
        }, requestOptions).then((value) => value.data),
        get: (input, requestOptions) => request({
          method: "GET",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/form/${encodeURIComponent(input.formID)}`,
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions).then((value) => value.data),
        reply: (input, requestOptions) => request({
          method: "POST",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/form/${encodeURIComponent(input.formID)}/reply`,
          body: { answer: input["answer"] },
          successStatus: 204,
          declaredStatuses: [400, 401, 404, 409],
          empty: true
        }, requestOptions),
        cancel: (input, requestOptions) => request({
          method: "DELETE",
          path: `/api/session/${encodeURIComponent(input.sessionID)}/form/${encodeURIComponent(input.formID)}`,
          query: { message: input["message"] },
          successStatus: 204,
          declaredStatuses: [400, 401, 404, 409],
          empty: true
        }, requestOptions)
      },
      environment: (input, requestOptions) => request({
        method: "PUT",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/environment`,
        body: { variables: input["variables"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      view: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/view`,
        body: { idle: input["idle"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions)
    },
    message: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/message`,
        query: { limit: input["limit"], order: input["order"], cursor: input["cursor"], type: input["type"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 500],
        empty: false
      }, requestOptions)
    },
    model: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/model`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 503],
        empty: false
      }, requestOptions),
      default: (input, requestOptions) => request({
        method: "GET",
        path: `/api/model/default`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 503],
        empty: false
      }, requestOptions)
    },
    generate: {
      text: (input, requestOptions) => request({
        method: "POST",
        path: `/api/experimental/generate`,
        body: { prompt: input["prompt"], model: input["model"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 503],
        empty: false
      }, requestOptions).then((value) => value.data)
    },
    voice: {
      session: (input, requestOptions) => request({
        method: "POST",
        path: `/api/experimental/voice/session`,
        body: { model: input["model"], session: input["session"], transport: input["transport"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 503],
        empty: false
      }, requestOptions).then((value) => value.data)
    },
    provider: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/provider`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 503],
        empty: false
      }, requestOptions),
      get: (input, requestOptions) => request({
        method: "GET",
        path: `/api/provider/${encodeURIComponent(input.providerID)}`,
        query: { location: input["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 503],
        empty: false
      }, requestOptions)
    },
    integration: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/integration`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      get: (input, requestOptions) => request({
        method: "GET",
        path: `/api/integration/${encodeURIComponent(input.integrationID)}`,
        query: { location: input["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      wellknown: {
        add: (input, requestOptions) => request({
          method: "POST",
          path: `/api/experimental/integration/wellknown`,
          query: { location: input["location"] },
          body: { url: input["url"] },
          successStatus: 204,
          declaredStatuses: [400, 401, 404],
          empty: true
        }, requestOptions)
      },
      connect: {
        key: (input, requestOptions) => request({
          method: "POST",
          path: `/api/integration/${encodeURIComponent(input.integrationID)}/connect/key`,
          query: { location: input["location"] },
          body: { key: input["key"], answer: input["answer"], label: input["label"] },
          successStatus: 204,
          declaredStatuses: [400, 401, 404],
          empty: true
        }, requestOptions),
        external: (input, requestOptions) => request({
          method: "POST",
          path: `/api/integration/${encodeURIComponent(input.integrationID)}/connect/external`,
          query: { location: input["location"] },
          body: { methodID: input["methodID"], answer: input["answer"], label: input["label"] },
          successStatus: 204,
          declaredStatuses: [400, 401, 404],
          empty: true
        }, requestOptions)
      },
      oauth: {
        connect: (input, requestOptions) => request({
          method: "POST",
          path: `/api/integration/${encodeURIComponent(input.integrationID)}/connect/oauth`,
          query: { location: input["location"] },
          body: { methodID: input["methodID"], answer: input["answer"], label: input["label"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions),
        status: (input, requestOptions) => request({
          method: "GET",
          path: `/api/integration/${encodeURIComponent(input.integrationID)}/connect/oauth/${encodeURIComponent(input.attemptID)}`,
          query: { location: input["location"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions),
        complete: (input, requestOptions) => request({
          method: "POST",
          path: `/api/integration/${encodeURIComponent(input.integrationID)}/connect/oauth/${encodeURIComponent(input.attemptID)}/complete`,
          query: { location: input["location"] },
          body: { code: input["code"] },
          successStatus: 204,
          declaredStatuses: [400, 401, 404],
          empty: true
        }, requestOptions),
        cancel: (input, requestOptions) => request({
          method: "DELETE",
          path: `/api/integration/${encodeURIComponent(input.integrationID)}/connect/oauth/${encodeURIComponent(input.attemptID)}`,
          query: { location: input["location"] },
          successStatus: 204,
          declaredStatuses: [400, 401, 404],
          empty: true
        }, requestOptions)
      },
      command: {
        connect: (input, requestOptions) => request({
          method: "POST",
          path: `/api/integration/${encodeURIComponent(input.integrationID)}/connect/command`,
          query: { location: input["location"] },
          body: { methodID: input["methodID"], label: input["label"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions),
        status: (input, requestOptions) => request({
          method: "GET",
          path: `/api/integration/${encodeURIComponent(input.integrationID)}/connect/command/${encodeURIComponent(input.attemptID)}`,
          query: { location: input["location"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions),
        cancel: (input, requestOptions) => request({
          method: "DELETE",
          path: `/api/integration/${encodeURIComponent(input.integrationID)}/connect/command/${encodeURIComponent(input.attemptID)}`,
          query: { location: input["location"] },
          successStatus: 204,
          declaredStatuses: [400, 401, 404],
          empty: true
        }, requestOptions)
      }
    },
    mcp: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/mcp`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      add: (input, requestOptions) => request({
        method: "PUT",
        path: `/api/experimental/mcp/${encodeURIComponent(input.server)}`,
        query: { location: input["location"] },
        body: { config: input["config"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      remove: (input, requestOptions) => request({
        method: "DELETE",
        path: `/api/experimental/mcp/${encodeURIComponent(input.server)}`,
        query: { location: input["location"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      connect: (input, requestOptions) => request({
        method: "POST",
        path: `/api/experimental/mcp/${encodeURIComponent(input.server)}/connect`,
        query: { location: input["location"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      disconnect: (input, requestOptions) => request({
        method: "POST",
        path: `/api/experimental/mcp/${encodeURIComponent(input.server)}/disconnect`,
        query: { location: input["location"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      resource: {
        catalog: (input, requestOptions) => request({
          method: "GET",
          path: `/api/mcp/resource`,
          query: { location: input?.["location"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions)
      }
    },
    credential: {
      list: (requestOptions) => request({ method: "GET", path: `/api/credential`, successStatus: 200, declaredStatuses: [400, 401], empty: false }, requestOptions).then((value) => value.data),
      create: (input, requestOptions) => request({
        method: "POST",
        path: `/api/credential`,
        body: {
          id: input["id"],
          integrationID: input["integrationID"],
          label: input["label"],
          value: input["value"],
          activate: input["activate"]
        },
        successStatus: 200,
        declaredStatuses: [400, 401, 409],
        empty: false
      }, requestOptions).then((value) => value.data),
      update: (input, requestOptions) => request({
        method: "PATCH",
        path: `/api/credential/${encodeURIComponent(input.credentialID)}`,
        body: { label: input["label"] },
        successStatus: 204,
        declaredStatuses: [400, 401],
        empty: true
      }, requestOptions),
      activate: (input, requestOptions) => request({
        method: "POST",
        path: `/api/credential/${encodeURIComponent(input.credentialID)}/activate`,
        successStatus: 204,
        declaredStatuses: [400, 401],
        empty: true
      }, requestOptions),
      remove: (input, requestOptions) => request({
        method: "DELETE",
        path: `/api/credential/${encodeURIComponent(input.credentialID)}`,
        successStatus: 204,
        declaredStatuses: [400, 401],
        empty: true
      }, requestOptions)
    },
    project: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/project`,
        query: { archived: input?.["archived"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      update: (input, requestOptions) => request({
        method: "PATCH",
        path: `/api/project/${encodeURIComponent(input.projectID)}`,
        body: {
          canonical: input["canonical"],
          name: input["name"],
          icon: input["icon"],
          commands: input["commands"]
        },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions)
    },
    form: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/form`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions)
    },
    permission: {
      request: {
        list: (input, requestOptions) => request({
          method: "GET",
          path: `/api/permission/request`,
          query: { location: input?.["location"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions)
      },
      saved: {
        list: (input, requestOptions) => request({
          method: "GET",
          path: `/api/permission/saved`,
          query: { projectID: input?.["projectID"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions).then((value) => value.data),
        update: (input, requestOptions) => request({
          method: "PATCH",
          path: `/api/permission/saved/${encodeURIComponent(input.id)}`,
          body: { action: input["action"], resource: input["resource"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404, 409],
          empty: false
        }, requestOptions).then((value) => value.data),
        remove: (input, requestOptions) => request({
          method: "DELETE",
          path: `/api/permission/saved/${encodeURIComponent(input.id)}`,
          successStatus: 204,
          declaredStatuses: [400, 401, 404],
          empty: true
        }, requestOptions)
      },
      create: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/permission`,
        body: {
          id: input["id"],
          action: input["action"],
          resources: input["resources"],
          save: input["save"],
          metadata: input["metadata"],
          source: input["source"],
          agent: input["agent"]
        },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions).then((value) => value.data),
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/permission`,
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions).then((value) => value.data),
      get: (input, requestOptions) => request({
        method: "GET",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/permission/${encodeURIComponent(input.requestID)}`,
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions).then((value) => value.data),
      reply: (input, requestOptions) => request({
        method: "POST",
        path: `/api/session/${encodeURIComponent(input.sessionID)}/permission/${encodeURIComponent(input.requestID)}/reply`,
        body: { decision: input["decision"], message: input["message"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions)
    },
    file: {
      read: (input, requestOptions) => request({
        method: "GET",
        path: `/api/fs/read/${encodePath(input.path)}`,
        query: { location: input["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false,
        binary: true
      }, requestOptions),
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/fs/list`,
        query: { location: input?.["location"], path: input?.["path"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      find: (input, requestOptions) => request({
        method: "GET",
        path: `/api/fs/find`,
        query: { location: input["location"], query: input["query"], type: input["type"], limit: input["limit"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      write: (input, requestOptions) => request({
        method: "POST",
        path: `/api/experimental/fs/write`,
        query: { location: input["location"], path: input["path"] },
        body: input["payload"],
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false,
        binaryBody: true
      }, requestOptions)
    },
    command: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/command`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions)
    },
    skill: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/skill`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions)
    },
    rpc: {
      call: (input, requestOptions) => request({
        method: "POST",
        path: `/api/rpc/${encodeURIComponent(input.rpcID)}/${encodeURIComponent(input.method)}`,
        query: { location: input["location"] },
        body: { input: input["input"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 500],
        empty: false
      }, requestOptions)
    },
    event: {
      subscribe: (requestOptions) => sse({ method: "GET", path: `/api/event`, successStatus: 200, declaredStatuses: [400, 401], empty: false }, requestOptions)
    },
    pty: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/pty`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      create: (input, requestOptions) => request({
        method: "POST",
        path: `/api/pty`,
        query: { location: input?.["location"] },
        body: {
          command: input?.["command"],
          args: input?.["args"],
          cwd: input?.["cwd"],
          title: input?.["title"],
          env: input?.["env"]
        },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      get: (input, requestOptions) => request({
        method: "GET",
        path: `/api/pty/${encodeURIComponent(input.ptyID)}`,
        query: { location: input["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      update: (input, requestOptions) => request({
        method: "PUT",
        path: `/api/pty/${encodeURIComponent(input.ptyID)}`,
        query: { location: input["location"] },
        body: { title: input["title"], size: input["size"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      remove: (input, requestOptions) => request({
        method: "DELETE",
        path: `/api/pty/${encodeURIComponent(input.ptyID)}`,
        query: { location: input["location"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      connect: {
        token: (input, requestOptions) => request({
          method: "POST",
          path: `/api/pty/${encodeURIComponent(input.ptyID)}/connect-token`,
          query: { location: input["location"] },
          headers: { "x-opencode-ticket": input["x-opencode-ticket"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 403, 404],
          empty: false
        }, requestOptions)
      }
    },
    experimental: {
      persistentPty: {
        read: (input, requestOptions) => request({
          method: "GET",
          path: `/api/experimental/session/${encodeURIComponent(input.sessionID)}/terminal/read`,
          query: { lines: input["lines"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 503],
          empty: false
        }, requestOptions).then((value) => value.data),
        list: (input, requestOptions) => request({
          method: "GET",
          path: `/api/experimental/session/${encodeURIComponent(input.sessionID)}/terminal`,
          successStatus: 200,
          declaredStatuses: [400, 401, 503],
          empty: false
        }, requestOptions).then((value) => value.data),
        create: (input, requestOptions) => request({
          method: "POST",
          path: `/api/experimental/session/${encodeURIComponent(input.sessionID)}/terminal`,
          body: {
            command: input["command"],
            args: input["args"],
            cwd: input["cwd"],
            title: input["title"],
            env: input["env"],
            size: input["size"]
          },
          successStatus: 200,
          declaredStatuses: [400, 401, 503],
          empty: false
        }, requestOptions).then((value) => value.data),
        shutdown: (requestOptions) => request({
          method: "POST",
          path: `/api/experimental/persistent-pty/shutdown`,
          successStatus: 204,
          declaredStatuses: [400, 401, 503],
          empty: true
        }, requestOptions),
        handoff: (requestOptions) => request({
          method: "POST",
          path: `/api/experimental/persistent-pty/handoff`,
          successStatus: 200,
          declaredStatuses: [400, 401, 503],
          empty: false
        }, requestOptions),
        get: (input, requestOptions) => request({
          method: "GET",
          path: `/api/experimental/persistent-pty/${encodeURIComponent(input.ptyID)}`,
          successStatus: 200,
          declaredStatuses: [400, 401, 404, 503],
          empty: false
        }, requestOptions).then((value) => value.data),
        update: (input, requestOptions) => request({
          method: "PUT",
          path: `/api/experimental/persistent-pty/${encodeURIComponent(input.ptyID)}`,
          body: { attachmentID: input["attachmentID"], size: input["size"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404, 503],
          empty: false
        }, requestOptions).then((value) => value.data),
        snapshot: (input, requestOptions) => request({
          method: "GET",
          path: `/api/experimental/persistent-pty/${encodeURIComponent(input.ptyID)}/snapshot`,
          successStatus: 200,
          declaredStatuses: [400, 401, 404, 503],
          empty: false
        }, requestOptions).then((value) => value.data),
        remove: (input, requestOptions) => request({
          method: "DELETE",
          path: `/api/experimental/persistent-pty/${encodeURIComponent(input.ptyID)}`,
          successStatus: 204,
          declaredStatuses: [400, 401, 404, 503],
          empty: true
        }, requestOptions),
        connectToken: (input, requestOptions) => request({
          method: "POST",
          path: `/api/experimental/persistent-pty/${encodeURIComponent(input.ptyID)}/connect-token`,
          headers: { "x-opencode-ticket": input["x-opencode-ticket"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 403, 404, 503],
          empty: false
        }, requestOptions).then((value) => value.data)
      }
    },
    shell: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/shell`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      create: (input, requestOptions) => request({
        method: "POST",
        path: `/api/shell`,
        query: { location: input["location"] },
        body: {
          command: input["command"],
          cwd: input["cwd"],
          timeout: input["timeout"],
          metadata: input["metadata"]
        },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      get: (input, requestOptions) => request({
        method: "GET",
        path: `/api/shell/${encodeURIComponent(input.id)}`,
        query: { location: input["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      output: (input, requestOptions) => request({
        method: "GET",
        path: `/api/shell/${encodeURIComponent(input.id)}/output`,
        query: { location: input["location"], cursor: input["cursor"], limit: input["limit"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      remove: (input, requestOptions) => request({
        method: "DELETE",
        path: `/api/shell/${encodeURIComponent(input.id)}`,
        query: { location: input["location"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions)
    },
    reference: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/reference`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions)
    },
    worktree: {
      list: (input, requestOptions) => request({
        method: "GET",
        path: `/api/worktree`,
        query: { projectID: input["projectID"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      create: (input, requestOptions) => request({
        method: "POST",
        path: `/api/worktree`,
        body: {
          projectID: input["projectID"],
          from: input["from"],
          branch: input["branch"],
          directory: input["directory"],
          name: input["name"]
        },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      remove: (input, requestOptions) => request({
        method: "DELETE",
        path: `/api/worktree`,
        body: { projectID: input["projectID"], directory: input["directory"], force: input["force"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions),
      refresh: (input, requestOptions) => request({
        method: "POST",
        path: `/api/worktree/refresh`,
        body: { projectID: input["projectID"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions)
    },
    vcs: {
      init: (input, requestOptions) => request({
        method: "POST",
        path: `/api/vcs/init`,
        query: { location: input?.["location"], provider: input?.["provider"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404, 409, 501, 503],
        empty: true
      }, requestOptions),
      get: (input, requestOptions) => request({
        method: "GET",
        path: `/api/vcs`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      base: (input, requestOptions) => request({
        method: "GET",
        path: `/api/vcs/base`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 503],
        empty: false
      }, requestOptions),
      status: (input, requestOptions) => request({
        method: "GET",
        path: `/api/vcs/status`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      branch: {
        list: (input, requestOptions) => request({
          method: "GET",
          path: `/api/vcs/branch`,
          query: { location: input?.["location"], search: input?.["search"], limit: input?.["limit"] },
          successStatus: 200,
          declaredStatuses: [400, 401, 404],
          empty: false
        }, requestOptions)
      },
      diff: (input, requestOptions) => request({
        method: "GET",
        path: `/api/vcs/diff`,
        query: { location: input["location"], mode: input["mode"], base: input["base"], context: input["context"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 503],
        empty: false
      }, requestOptions)
    },
    debug: {
      location: {
        list: (requestOptions) => request({
          method: "GET",
          path: `/api/debug/location`,
          successStatus: 200,
          declaredStatuses: [400, 401],
          empty: false
        }, requestOptions),
        evict: (input, requestOptions) => request({
          method: "DELETE",
          path: `/api/debug/location`,
          query: { location: input?.["location"] },
          successStatus: 204,
          declaredStatuses: [400, 401],
          empty: true
        }, requestOptions)
      }
    },
    migration: {
      v1: {
        status: (requestOptions) => request({
          method: "GET",
          path: `/api/experimental/migration/v1`,
          successStatus: 200,
          declaredStatuses: [400, 401],
          empty: false
        }, requestOptions)
      }
    },
    websearch: {
      providers: (input, requestOptions) => request({
        method: "GET",
        path: `/api/websearch/provider`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 503],
        empty: false
      }, requestOptions),
      query: (input, requestOptions) => request({
        method: "POST",
        path: `/api/websearch`,
        query: { location: input["location"] },
        body: { query: input["query"], providerID: input["providerID"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404, 503],
        empty: false
      }, requestOptions)
    },
    config: {
      get: (input, requestOptions) => request({
        method: "GET",
        path: `/api/config`,
        query: { location: input?.["location"] },
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      shells: (requestOptions) => request({
        method: "GET",
        path: `/api/config/shell`,
        successStatus: 200,
        declaredStatuses: [400, 401, 404],
        empty: false
      }, requestOptions),
      update: (input, requestOptions) => request({
        method: "PATCH",
        path: `/api/experimental/config`,
        body: { shell: input["shell"] },
        successStatus: 204,
        declaredStatuses: [400, 401, 404],
        empty: true
      }, requestOptions)
    }
  };
}
function encodePath(value) {
  return value.split("/").map(encodeURIComponent).join("/");
}
function appendQuery(params, key, value) {
  if (value === undefined)
    return;
  if (value === null) {
    params.append(key, "null");
    return;
  }
  if (Array.isArray(value)) {
    for (const item of value)
      appendQuery(params, key, item);
    return;
  }
  if (typeof value === "object") {
    for (const [child, item] of Object.entries(value))
      appendQuery(params, `${key}[${child}]`, item);
    return;
  }
  params.append(key, String(value));
}
async function json(response) {
  if (!isContentType(response, "application/json") && !response.headers.get("content-type")?.includes("+json")) {
    try {
      await response.body?.cancel();
    } catch {}
    throw new ClientError3("UnsupportedContentType", { detail: response.headers.get("content-type") });
  }
  let text;
  try {
    text = await response.text();
  } catch (cause) {
    throw new ClientError3("Transport", { cause });
  }
  if (text === "")
    throw new ClientError3("MalformedResponse");
  try {
    return JSON.parse(text);
  } catch (cause) {
    throw new ClientError3("MalformedResponse", { cause });
  }
}
function declared(body) {
  const error = Object.assign(new Error(body.message ?? body.data?.message), body);
  if (body._tag)
    error.name = body._tag;
  return error;
}
function isContentType(response, expected) {
  return response.headers.get("content-type")?.split(";", 1)[0]?.trim().toLowerCase() === expected;
}

export { make6, exports_client4 };
