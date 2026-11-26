"use strict";

const byID = (id) => document.getElementById(id);
const login = byID("login-form");
const composer = byID("composer");
const connection = byID("connection");
let current;

function element(tag, value, className) {
  const node = document.createElement(tag);
  node.textContent = value;
  if (className) node.className = className;
  return node;
}

async function request(session, path, options = {}) {
  const headers = new Headers(options.headers);
  if (session) headers.set("Authorization", `Bearer ${session.token}`);
  const response = await fetch(path, { ...options, headers, cache: "no-store" });
  if (!response.ok) {
    const problem = await response.json().catch(() => ({}));
    const error = new Error(`请求失败（${response.status}${problem.error ? ` · ${problem.error}` : ""}）`);
    error.status = response.status;
    error.code = problem.error;
    throw error;
  }
  return response;
}

async function json(session, path, method = "GET", data) {
  const options = { method };
  if (data !== undefined) {
    options.headers = { "Content-Type": "application/json" };
    options.body = JSON.stringify(data);
  }
  return (await request(session, path, options)).json();
}

function feedback(message = "") {
  byID("feedback").textContent = message;
}

function bindForm(form, handler) {
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = form.querySelector('button[type="submit"]');
    button.disabled = true;
    feedback();
    try {
      await handler(new FormData(form));
    } catch (error) {
      feedback(error.message);
    } finally {
      button.disabled = false;
    }
  });
}

function action(node, handler) {
  node.addEventListener("click", async () => {
    node.disabled = true;
    feedback();
    try {
      await handler();
    } catch (error) {
      feedback(error.message);
    } finally {
      node.disabled = false;
    }
  });
}

function idFrom(value) {
  const id = Number(value);
  if (!Number.isSafeInteger(id) || id <= 0) throw new Error("请输入有效的用户 ID。");
  return id;
}

function base(id) {
  return `/v1/conversations/${id}`;
}

function title(room, user) {
  if (room.kind === "group") return room.title;
  const peer = room.direct_low === user ? room.direct_high : room.direct_low;
  return `与用户 ${peer} 的私聊`;
}

async function collection(session, path, gaps) {
  const items = [];
  let after = 0;
  do {
    const page = await json(session, `${path}?after=${after}`);
    items.push(...(page.items || []));
    if (gaps) gaps.push(...(page.expired || []));
    const next = page.has_more === false ? 0 : (page.next_after || 0);
    if (next && next <= after) throw new Error("同步游标没有前进。");
    after = next;
  } while (after);
  return items;
}

async function update(session) {
  const [rooms, inbox] = await Promise.all([collection(session, "/v1/conversations"), collection(session, "/v1/inbox")]);
  if (current !== session) return;
  const summaries = new Map(inbox.map((item) => [item.conversation_id, item]));
  const selected = session.selected;
  const room = rooms.find((item) => item.id === selected);
  byID("rooms").replaceChildren(...rooms.map((item) => {
    const li = document.createElement("li");
    const button = element("button", title(item, session.profile.user_id));
    button.type = "button";
    button.setAttribute("aria-current", String(item.id === selected));
    const summary = summaries.get(item.id);
    if (summary) button.append(element("small", `最新序号 ${summary.high_seq} · 已读 ${summary.read_seq}`));
    action(button, () => selectRoom(session, item.id));
    li.append(button);
    return li;
  }));
  byID("empty").hidden = Boolean(room);
  byID("opened").hidden = !room;
  if (!room) return;

  const path = base(selected);
  const gaps = [];
  const [messages, read] = await Promise.all([collection(session, `${path}/sync`, gaps), json(session, `${path}/read`)]);
  if (current !== session || session.selected !== selected) return;
  const entry = summaries.get(selected);
  const high = messages.at(-1)?.seq || 0;
  session.high = high;
  let sync = entry?.sync_seq || 0;
  if (high > sync) sync = (await json(session, `${path}/sync`, "PUT", { seq: high })).sync_seq;
  if (current !== session || session.selected !== selected) return;
  byID("room-name").textContent = title(room, session.profile.user_id);
  const gapLabel = gaps.length ? ` · 历史已过期 ${gaps.map((gap) => `${gap.from}–${gap.to}`).join("、")}` : "";
  byID("room-state").textContent = `会话 ${selected} · 设备游标 ${sync} · 已读 ${read.read_seq} · 未读 ${read.unread}${gapLabel}`;
  byID("add-member").hidden = room.kind !== "group";
  byID("membership-tools").hidden = room.kind !== "group";
  byID("mark-read").disabled = !high || high <= read.read_seq;
  renderMessages(session, messages);
}

function refresh(session) {
  session.dirty = true;
  if (!session.refreshing) {
    session.refreshing = (async () => {
      while (current === session && session.dirty) {
        session.dirty = false;
        try {
          await update(session);
        } catch (error) {
          if (current === session) feedback(error.message);
        }
      }
    })().finally(() => { session.refreshing = null; });
  }
  return session.refreshing;
}

function selectRoom(session, id) {
  if (session.selected !== id) {
    session.selected = id;
    session.high = 0;
    composer.reset();
    renderPending(session);
    const memberTools = byID("membership-tools");
    memberTools.open = false;
    resetMembershipTools();
    byID("messages").replaceChildren();
    byID("room-name").textContent = "正在同步";
    byID("room-state").textContent = "";
    byID("opened").hidden = true;
  }
  return refresh(session);
}

function renderMessages(session, messages) {
  const list = byID("messages");
  const atEnd = list.scrollHeight - list.clientHeight - list.scrollTop < 36;
  list.replaceChildren(...messages.map((message) => {
    const self = message.sender_id === session.profile.user_id;
    const item = element("li", "", self ? "self" : "");
    item.append(element("div", `#${message.seq} · ${self ? "我" : `用户 ${message.sender_id}`}`, "message-meta"));
    item.append(element("div", message.retracted_at ? "消息已撤回" : message.expired ? "消息已过期" : message.body, "message-body"));
    const actions = element("div", "", "message-actions");
    for (const id of message.attachment_ids || []) {
      const button = element("button", `下载附件 ${id}`);
      action(button, () => download(session, message, id));
      actions.append(button);
    }
    if (self && !message.retracted_at && !message.expired) {
      const button = element("button", "撤回");
      action(button, async () => {
        await json(session, `${base(message.conversation_id)}/messages/${message.seq}`, "DELETE");
        await refresh(session);
      });
      actions.append(button);
    }
    if (actions.childElementCount) item.append(actions);
    return item;
  }));
  if (atEnd) list.scrollTop = list.scrollHeight;
}

async function download(session, message, attachment) {
  const grant = await json(session, `${base(message.conversation_id)}/messages/${message.seq}/attachments/${attachment}/grants`, "POST");
  const response = await request(null, "/v1/attachment-content", { headers: { Authorization: `Attachment ${grant.token}` } });
  const url = URL.createObjectURL(await response.blob());
  const link = document.createElement("a");
  link.href = url;
  link.download = `attachment-${attachment}`;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

async function connect(session) {
  if (connection.dataset.state === "connecting" || session.socket?.readyState === WebSocket.OPEN) return;
  connection.dataset.state = "connecting";
  connection.textContent = "正在接入";
  try {
    const { ticket } = await json(session, "/v1/gateway/tickets", "POST");
    if (current !== session) return;
    const url = new URL("/v1/gateway", location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(url, ["messaging.v1", `ticket.${ticket}`]);
    session.socket = socket;
    socket.addEventListener("message", ({ data }) => {
      if (current !== session) return;
      const frame = JSON.parse(data);
      if (frame.type === "ready") {
        connection.dataset.state = "on";
        connection.textContent = `${frame.node.name} · 连接代数 ${frame.connection_epoch}`;
        refresh(session);
      } else if (frame.type === "notification.resync_required") {
        feedback("正在补拉消息。刷新可重建实时连接。");
        socket.close();
        refresh(session);
      } else if (frame.type.startsWith("notification.")) {
        socket.send(JSON.stringify({ type: "notification.ack", delivery_id: frame.delivery_id }));
        refresh(session);
      }
    });
    socket.addEventListener("close", () => {
      if (current !== session || socket !== session.socket) return;
      connection.dataset.state = "off";
      connection.textContent = "连接已断开";
    });
  } catch (error) {
    if (current === session) {
      connection.dataset.state = "off";
      connection.textContent = "连接未建立";
    }
    throw error;
  }
}

bindForm(login, async (data) => {
  const credentials = Object.fromEntries(data);
  const { token, session: profile } = await json(null, "/v1/sessions", "POST", credentials);
  const session = { token, profile, selected: 0, high: 0, activeSends: new Set() };
  current = session;
  login.elements.password.value = "";
  byID("signin").hidden = true;
  byID("workspace").hidden = false;
  byID("logout").hidden = false;
  byID("accounts").textContent = `${profile.display_name} · 用户 ${profile.user_id} · 设备 ${profile.device_key}`;
  await refresh(session);
  await connect(session);
});

action(byID("logout"), async () => {
  const session = current;
  current = undefined;
  session.socket?.close();
  byID("signin").hidden = false;
  byID("workspace").hidden = true;
  byID("logout").hidden = true;
  byID("messages").replaceChildren();
  byID("rooms").replaceChildren();
  byID("opened").hidden = true;
  byID("empty").hidden = false;
  composer.reset();
  connection.dataset.state = "off";
  connection.textContent = "未连接";
  login.elements.username.focus();
  await request(session, `/v1/devices/${session.profile.device_id}`, { method: "DELETE" });
});

action(byID("reload"), async () => {
  const session = current;
  await refresh(session);
  if (current === session) await connect(session);
});

const direct = byID("create-direct");
bindForm(direct, async (data) => {
  const session = current;
  const room = await json(session, "/v1/conversations/direct", "POST", { user_id: idFrom(data.get("user_id")) });
  if (current !== session) return;
  direct.reset();
  await selectRoom(session, room.id);
});

const group = byID("create-group");
bindForm(group, async (data) => {
  const session = current;
  const room = await json(session, "/v1/conversations/group", "POST", { title: data.get("title").trim(), history_policy: "since_join" });
  if (current !== session) return;
  group.reset();
  await selectRoom(session, room.id);
});

const member = byID("add-member");
bindForm(member, async (data) => {
  const session = current;
  const room = session.selected;
  await json(session, `${base(room)}/members/${idFrom(data.get("user_id"))}`, "PUT", { role: "member" });
  if (current === session && room === session.selected) {
    member.reset();
    if (byID("membership-tools").open) await loadMembershipOperations(session, room);
  }
});

const memberTools = byID("membership-tools");
const memberBatchForm = byID("membership-batch");
const memberOperationList = byID("membership-operation-list");
const olderMemberOperations = byID("older-membership-operations");

function resetMembershipTools() {
  byID("membership-content").hidden = true;
  byID("membership-loading").textContent = "";
  memberBatchForm.reset();
  memberOperationList.replaceChildren();
  olderMemberOperations.hidden = true;
}

function membershipToolsCurrent(session, room) {
  return current === session && room === session.selected && memberTools.open;
}

async function loadMembershipOperations(session, room, before = 0) {
  const loading = byID("membership-loading");
  loading.textContent = "正在查询操作记录";
  try {
    const log = await json(session, `${base(room)}/membership-operations${before ? `?before=${before}` : ""}`);
    if (!membershipToolsCurrent(session, room)) return;
    if (!before) memberOperationList.replaceChildren();
    memberOperationList.append(...log.items.map((operation) => renderMembershipOperation(session, room, operation)));
    loading.textContent = memberOperationList.childElementCount ? "" : "暂无成员操作记录";
    byID("membership-content").hidden = false;
    olderMemberOperations.value = log.next_before || "";
    olderMemberOperations.hidden = !log.next_before;
  } catch (error) {
    if (!membershipToolsCurrent(session, room)) return;
    if (error.status === 404) byID("membership-content").hidden = true;
    loading.textContent = error.status === 404 ? "当前设备账号没有群成员管理权限。" : error.message;
  }
}

memberTools.addEventListener("toggle", () => {
  if (memberTools.open && current) loadMembershipOperations(current, current.selected);
});

action(olderMemberOperations, () => loadMembershipOperations(current, current.selected, Number(olderMemberOperations.value)));

function membershipChangeLabel(change) {
  if (change.action === "remove") return "移除";
  if (change.action === "transfer") return "设为群主";
  return change.role === "admin" ? "设为管理员" : "设为成员";
}

function renderMembershipOperation(session, room, operation) {
  const card = element("section", "", "membership-operation");
  const roles = { owner: "群主", admin: "管理员", member: "成员" };
  const ownBatch = operation.kind === "batch" && session.profile.user_id === operation.actor_id;
  card.append(element("p", `#${operation.id} · 版本 ${operation.version} · ${roles[operation.actor_role]} ${operation.actor_id} · 设备 ${operation.device_id} · ${new Date(operation.completed_at || operation.created_at).toLocaleString()}`));
  const rows = element("ul", "");
  const failed = [];
  for (const { status, reason, ...change } of operation.items) {
    const state = { not_started: "未开始", failed: "失败", succeeded: "成功" }[status];
    const detail = reason === "forbidden" ? "（权限或群角色限制）" : reason === "not_found" ? "（当前应用或群组内找不到用户）" : "";
    const label = `用户 ${change.user_id} · ${membershipChangeLabel(change)} · ${state}${detail}`;
    const row = element("li", "", status === "failed" ? "member-failed" : "");
    if (status === "failed") {
      const checkbox = element("input", "");
      checkbox.type = "checkbox";
      const wrapper = element("label", "");
      wrapper.append(checkbox, document.createTextNode(label));
      row.append(wrapper);
      failed.push({ checkbox, change });
    } else {
      row.textContent = label;
    }
    rows.append(row);
  }
  card.append(rows);
  if (ownBatch && operation.version === 1) {
    const button = element("button", "执行此批次");
    action(button, async () => {
      const endpoint = `${base(room)}/membership-batches/${operation.client_id}`;
      const receipt = await json(session, endpoint);
      if (receipt.version === 1) await json(session, `${endpoint}/execute`, "POST");
      if (membershipToolsCurrent(session, room)) await loadMembershipOperations(session, room);
    });
    card.append(button);
  }
  if (failed.length) {
    const button = element("button", "用勾选的失败项创建新批次");
    action(button, async () => {
      const items = failed.filter(({ checkbox }) => checkbox.checked).map(({ change }) => change);
      if (!items.length) throw new Error("请勾选要重试的失败项。");
      await prepareMembershipBatch(session, room, items);
    });
    card.append(button);
  }
  return card;
}

async function prepareMembershipBatch(session, room, items) {
  const { tenant_id, app_id, user_id } = session.profile;
  const key = `messaging:membership-batch:${tenant_id}:${app_id}:${user_id}:${room}`;
  const saved = JSON.parse(sessionStorage.getItem(key));
  const draft = saved && JSON.stringify(saved.items) === JSON.stringify(items) ? saved : { client_id: crypto.randomUUID(), items };
  sessionStorage.setItem(key, JSON.stringify(draft));
  await json(session, `${base(room)}/membership-batches`, "POST", draft);
  if (JSON.parse(sessionStorage.getItem(key))?.client_id === draft.client_id) sessionStorage.removeItem(key);
  if (membershipToolsCurrent(session, room)) {
    memberBatchForm.reset();
    await loadMembershipOperations(session, room);
  }
}

bindForm(memberBatchForm, async (data) => {
  const values = data.get("users").trim().split(/[\s,，]+/);
  if (values.length > 30 || values.some((value) => !/^[0-9]+$/.test(value))) throw new Error("每批请输入 1 至 30 个用户 ID。");
  const users = values.map(idFrom);
  if (new Set(users).size !== users.length) throw new Error("同一批次不能重复选择用户。");
  const operation = data.get("operation");
  const items = users.map((user_id) => operation === "remove" ? { user_id, action: "remove" } : { user_id, action: "set", role: operation });
  await prepareMembershipBatch(current, current.selected, items);
});

action(byID("mark-read"), async () => {
  const session = current;
  if (!session.high) return;
  await json(session, `${base(session.selected)}/read`, "PUT", { seq: session.high });
  await refresh(session);
});

async function upload(session, file) {
  if (!file.size || file.size > 10 * 1024 * 1024) throw new Error("附件必须介于 1 字节和 10 MiB 之间。");
  const payload = await file.arrayBuffer();
  const hash = await crypto.subtle.digest("SHA-256", payload);
  const sha256 = [...new Uint8Array(hash)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
  const attachment = await json(session, "/v1/attachments", "POST", { filename: file.name, content_type: "application/octet-stream", size: file.size, sha256 });
  await request(session, `/v1/attachments/${attachment.id}/content`, { method: "PUT", body: payload });
  return attachment.id;
}

function pendingKey(session, conversation) {
  const { tenant_id, app_id, user_id, device_id } = session.profile;
  return `messaging:pending:${tenant_id}:${app_id}:${user_id}:${device_id}:${conversation}`;
}

function pendingMessage(session, conversation) {
  return JSON.parse(sessionStorage.getItem(pendingKey(session, conversation)));
}

function renderPending(session) {
  const draft = pendingMessage(session, session.selected);
  const busy = session.activeSends.has(session.selected);
  const status = byID("send-status");
  status.hidden = byID("check-send").hidden = byID("clear-send").hidden = !draft;
  composer.elements.body.readOnly = Boolean(draft) || busy;
  composer.elements.file.disabled = Boolean(draft) || busy;
  byID("check-send").disabled = byID("clear-send").disabled = byID("send").disabled = busy;
  byID("send").textContent = draft ? "重试发送" : "发送";
  if (draft) {
    composer.elements.body.value = draft.body;
    status.textContent = `等待确认 · ${draft.client_id}`;
  }
}

function messageAction(task) {
  return async (data) => {
    const session = current;
    const conversation = session.selected;
    if (session.activeSends.has(conversation)) return;
    session.activeSends.add(conversation);
    try {
      renderPending(session);
      await task(session, conversation, pendingMessage(session, conversation), data);
    } catch (error) {
      if (current === session && session.selected === conversation) throw error;
    } finally {
      session.activeSends.delete(conversation);
      if (current === session && session.selected === conversation) renderPending(session);
    }
  };
}

function samePending(session, conversation, draft) {
  return pendingMessage(session, conversation)?.client_id === draft.client_id;
}

async function settlePending(session, conversation, draft, message) {
  if (!samePending(session, conversation, draft)) return;
  sessionStorage.removeItem(pendingKey(session, conversation));
  if (current !== session || session.selected !== conversation) return;
  composer.reset();
  renderPending(session);
  feedback(message ? `已落库 · #${message.seq}` : "已放弃待确认发送");
  if (message) await refresh(session);
}

async function recoverPending(session, conversation, draft) {
  const url = `${base(conversation)}/messages/by-client/${encodeURIComponent(draft.client_id)}`;
  try {
    const message = await json(session, url);
    await settlePending(session, conversation, draft, message);
    return true;
  } catch (error) {
    if (error.status === 404 && error.code === "not_found") return false;
    throw error;
  }
}

action(byID("check-send"), messageAction(async (session, conversation, draft) => {
  if (draft && !await recoverPending(session, conversation, draft) && current === session
      && session.selected === conversation && samePending(session, conversation, draft)) {
    feedback("尚未查到持久回执，可用原消息号重试。");
  }
}));

action(byID("clear-send"), messageAction(async (session, conversation, draft) => {
  if (draft && !await recoverPending(session, conversation, draft)) {
    await settlePending(session, conversation, draft, null);
  }
}));

bindForm(composer, messageAction(async (session, conversation, draft, data) => {
  if (draft) {
    if (await recoverPending(session, conversation, draft) || !samePending(session, conversation, draft)) return;
  } else {
    const body = data.get("body");
    const file = data.get("file");
    if (!body.trim() && !file.name) throw new Error("请输入消息或选择附件。");
    draft = { client_id: crypto.randomUUID(), body };
    if (file.name) draft.attachment_ids = [await upload(session, file)];
    if (current !== session || session.selected !== conversation) return;
    sessionStorage.setItem(pendingKey(session, conversation), JSON.stringify(draft));
    renderPending(session);
  }
  const receipt = await json(session, `${base(conversation)}/messages`, "POST", draft);
  await settlePending(session, conversation, draft, receipt.message);
}));
