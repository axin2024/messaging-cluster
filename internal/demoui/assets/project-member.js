import { $, action, displayDate, element, feedback, form, identifier, json, key, list, request, rowButton, saveResponse } from "./project-common.js";

const member = { token: "", links: [], project: null, channels: [], channel: null, thread: null, quote: null };
let timelineCursor = "";
let threadCursor = "";
let searchCursor = "";
let searchQuery = "";

function resetSearch() {
  searchCursor = "";
  searchQuery = "";
  $("search-results").hidden = true;
  $("search-list").replaceChildren();
  $("search-more").hidden = true;
}

function resetThread() {
  member.thread = null;
  member.quote = null;
  threadCursor = "";
  $("thread").hidden = true;
  $("thread-messages").replaceChildren();
  $("mention-candidates").replaceChildren();
  $("clear-reference").hidden = true;
  $("compose-title").textContent = "新消息";
}

function resetChannel() {
  member.channel = null;
  timelineCursor = "";
  $("member-channel").hidden = true;
  $("timeline").replaceChildren();
  $("timeline-more").hidden = true;
  $("mentions").hidden = true;
  $("mentions").replaceChildren();
  $("legacy-tools").hidden = true;
  $("legacy-preview").hidden = true;
  $("legacy-preview").replaceChildren();
  $("legacy-publication").reset();
  $("compose").reset();
  resetThread();
}

function resetProject() {
  member.project = null;
  member.channels = [];
  $("member-heading").textContent = "选择一个项目";
  $("authority").textContent = "";
  $("search").reset();
  $("search").hidden = true;
  resetSearch();
  resetChannel();
  $("member-channels").replaceChildren();
}

function renderMemberLinks() {
  list($("member-links"), member.links.map((link) => rowButton(
    `${link.space_title} · ${link.host_tenant} / ${link.peer_tenant}`, link.link_id === member.project?.link_id,
    () => chooseProject(link),
  )), "当前设备没有有效的项目授权。");
}

function renderChannels() {
  list($("member-channels"), member.channels.map((channel) => rowButton(
    `${channel.title} · ${channel.audience === "host" ? "内部" : "共享"}`, channel.channel_id === member.channel?.channel_id,
    () => chooseChannel(channel),
  )), "当前没有可见频道。");
}

async function access(operation) {
  const token = member.token;
  try {
    return await operation(token);
  } catch (error) {
    if (token !== member.token) throw error;
    if (error.status === 404 || error.status === 401) {
      resetProject();
      const links = error.status === 404 ? (await json(token, "/v1/shared-projects").catch(() => ({ items: [] }))).items : [];
      if (token === member.token) {
        member.links = links;
        renderMemberLinks();
      }
    } else if (error.code === "refresh_shared_cursor") {
      resetSearch();
      resetChannel();
      renderChannels();
    }
    throw error;
  }
}

async function refreshProjects(restore = false) {
  const token = member.token;
  const priorLink = member.project?.link_id;
  const priorChannel = member.channel?.channel_id;
  resetProject();
  const { items } = await access((token) => json(token, "/v1/shared-projects"));
  if (member.token !== token) return;
  member.links = items;
  renderMemberLinks();
  if (!restore || !priorLink) return;
  const link = items.find((link) => link.link_id === priorLink);
  if (!link) {
    feedback("重新鉴权完成。此前打开的项目已不可访问。", true);
    return;
  }
  await chooseProject(link);
  const channel = member.channels.find((channel) => channel.channel_id === priorChannel);
  if (channel) await chooseChannel(channel);
}

async function chooseProject(link) {
  resetProject();
  member.project = link;
  renderMemberLinks();
  $("member-heading").textContent = link.space_title;
  const days = Math.max(1, Math.ceil((new Date(link.expires_at).getTime() - Date.now()) / 86400000));
  $("authority").textContent = `合作还剩约 ${days} 天 · ${displayDate(link.expires_at)} 到期`;
  $("search").hidden = false;
  const page = await access((token) => json(token, `/v1/partner-links/${link.link_id}/channels`));
  if (member.project !== link) return;
  member.channels = page.items;
  renderChannels();
}

function messagePath(channel = member.channel) {
  return `${channelEndpoint(channel)}/messages`;
}

function channelEndpoint(channel = member.channel) {
  return `/v1/shared-channels/${channel.channel_id}`;
}

async function chooseChannel(channel) {
  resetChannel();
  member.channel = channel;
  renderChannels();
  $("member-channel").hidden = false;
  $("channel-title").textContent = channel.title;
  const host = `${member.project.host_tenant}/${member.project.host_app}`;
  const peer = `${member.project.peer_tenant}/${member.project.peer_app}`;
  const history = channel.history_from === "join" ? "从本人加入后读取" : "从频道最早处读取";
  $("message-audience").textContent = `接收组织 ${channel.audience === "host" ? host : `${host}、${peer}`} · ${history}`;
  $("legacy-tools").hidden = member.project.my_side !== "host" || channel.audience !== "shared";
  await loadTimeline();
}

function messageNode(item, thread = false) {
  const node = element("li", "");
  node.append(element("small", `#${item.seq} · 组织 ${item.sender_tenant} · 成员 ${item.sender_user} · ${displayDate(item.created_at)}`));
  if (item.root_seq) node.append(element("small", ` · 回复 #${item.root_seq}`));
  if (item.quote_seq) node.append(element("small", ` · 引用 #${item.quote_seq}`));
  node.append(element("p", item.body));
  const actions = element("div", "", "actions");
  if (!thread) actions.append(rowButton("打开讨论", undefined, () => openThread(item.root_seq || item.seq)));
  actions.append(rowButton("引用", undefined, () => {
    member.quote = item.seq;
    updateComposer();
    $("message-body").focus();
  }));
  for (const file of item.attachments || []) {
    actions.append(rowButton(`下载 ${file.filename}`, undefined, () => downloadFile(item.seq, file)));
  }
  node.append(actions);
  return node;
}

async function loadTimeline(append = false) {
  const channel = member.channel;
  if (!channel) return;
  const target = $("timeline");
  if (!append) target.replaceChildren();
  const cursor = append ? `?cursor=${encodeURIComponent(timelineCursor)}` : "";
  const page = await access((token) => json(token, `${messagePath(channel)}${cursor}`));
  if (member.channel !== channel) return;
  if (page.items.length) {
    target.append(...page.items.map((item) => messageNode(item)));
  } else if (!append) {
    target.append(element("li", "频道还没有当前授权可见的消息。", "muted"));
  }
  timelineCursor = page.cursor;
  $("authority").textContent = `授权代际 ${page.grant_epoch} · 合作 ${displayDate(member.project.expires_at)} 到期`;
  $("timeline-more").hidden = !page.has_more;
}

function updateComposer() {
  $("compose-title").textContent = member.thread ? `回复主题 #${member.thread}` : "新消息";
  if (member.quote) $("compose-title").textContent += ` · 引用 #${member.quote}`;
  $("clear-reference").hidden = !member.quote;
}

async function openThread(root) {
  resetThread();
  member.thread = root;
  $("thread-title").textContent = `主题 #${root}`;
  $("thread").hidden = false;
  updateComposer();
  await loadThread();
}

async function loadThread(append = false) {
  const { channel, thread } = member;
  if (!channel || !thread) return;
  const cursor = append ? `?cursor=${encodeURIComponent(threadCursor)}` : "";
  const page = await access((token) => json(token, `/v1/shared-channels/${channel.channel_id}/threads/${thread}${cursor}`));
  if (member.channel !== channel || member.thread !== thread) return;
  if (!append) $("thread-messages").replaceChildren();
  $("thread-messages").append(...page.items.map((item) => messageNode(item, true)));
  threadCursor = page.cursor;
  $("thread-more").hidden = !page.has_more;
}

async function search(append = false) {
  const project = member.project;
  const q = searchQuery;
  const params = new URLSearchParams({ q });
  if (append) params.set("cursor", searchCursor);
  else $("search-list").replaceChildren();
  const page = await access((token) => json(token, `/v1/partner-links/${project.link_id}/search?${params}`));
  if (member.project !== project || searchQuery !== q) return;
  $("search-results").hidden = false;
  $("search-more").hidden = !page.has_more;
  if (!page.items.length && !append) $("search-list").append(element("p", "没有匹配的可见内容。", "muted"));
  for (const item of page.items) {
    const row = element("article", "");
    row.append(element("small", `${project.space_title} / ${item.channel_title} · #${item.seq} · ${displayDate(item.created_at)}`), element("p", item.snippet));
    $("search-list").append(row);
  }
  searchCursor = page.search_cursor;
}

async function downloadFile(seq, file) {
  const path = `${messagePath()}/${seq}/attachments/${file.id}/grant`;
  await access(async (token) => {
    const pass = await json(token, path, "POST");
    const body = await request(token, "/v1/shared-attachment-content", { headers: { "X-Attachment-Token": pass.token } });
    if (token === member.token) await saveResponse(body, file.filename);
  });
}

async function previewMentions() {
  const { channel, thread } = member;
  const side = $("compose").elements.mention_side.value;
  const query = new URLSearchParams({ side });
  const role = $("compose").elements.mention_role.value.trim();
  if (role) query.set("duty_role", role);
  if (thread) query.set("root_seq", thread);
  const { items } = await access((token) => json(token, `${channelEndpoint(channel)}/mention-candidates?${query}`));
  if (member.channel !== channel || member.thread !== thread) return;
  list($("mention-candidates"), items.map((candidate) => {
    const check = element("input", "");
    check.type = "checkbox";
    check.name = "mention_user";
    check.value = candidate.user_id;
    const label = element("label", "", "check");
    label.append(check, document.createTextNode(`${candidate.display_name} · ${candidate.duty_role} · 用户 ${candidate.user_id}`));
    return label;
  }), "当前组织没有能看到此内容的其他成员。");
}

async function loadMentions() {
  const channel = member.channel;
  const { items } = await access((token) => json(token, `${channelEndpoint(channel)}/mentions`));
  if (member.channel !== channel) return;
  $("mentions").hidden = false;
  list($("mentions"), items.map((item) => rowButton(`消息 #${item.seq} · 来自用户 ${item.sender_user} · ${displayDate(item.created_at)}`,
    undefined, () => openThread(item.root_seq || item.seq))), "此频道没有可见提醒。");
}

async function uploadFile(token, file) {
  if (file.size < 1 || file.size > 10 * 1024 * 1024) throw new Error("附件必须大于 0 字节且不超过 10 MiB。");
  if (!crypto.subtle) throw new Error("附件上传需要 HTTPS 或本机回环地址。");
  const hashes = new Uint8Array(await crypto.subtle.digest("SHA-256", await file.arrayBuffer()));
  const sha256 = Array.from(hashes, (value) => value.toString(16).padStart(2, "0")).join("");
  const data = { filename: file.name, content_type: file.type || "application/octet-stream", size: file.size, sha256 };
  const attachment = await json(token, "/v1/attachments", "POST", data);
  await request(token, `/v1/attachments/${attachment.id}/content`, { method: "PUT", body: file });
  return attachment.id;
}

form("member-login", async (data) => {
  const response = await json("", "/v1/sessions", "POST", Object.fromEntries(data));
  member.token = response.token;
  $("member-login").reset();
  $("member-login").hidden = true;
  $("member-workspace").hidden = false;
  $("member-identity").textContent = `${response.session.display_name} · 用户 ${response.session.user_id} · ${response.session.device_key}`;
  await refreshProjects();
});

action($("member-logout"), () => {
  member.token = "";
  member.links = [];
  resetProject();
  renderMemberLinks();
  $("member-workspace").hidden = true;
  $("member-login").hidden = false;
});
action($("member-refresh"), () => refreshProjects(true));
action($("timeline-refresh"), () => loadTimeline());
action($("mentions-refresh"), loadMentions);
action($("preview-mentions"), previewMentions);
$("compose").elements.mention_side.addEventListener("change", () => $("mention-candidates").replaceChildren());
$("compose").elements.mention_role.addEventListener("input", () => $("mention-candidates").replaceChildren());
action($("timeline-more"), () => loadTimeline(true));
action($("thread-close"), resetThread);
action($("thread-more"), () => loadThread(true));
action($("clear-reference"), () => { member.quote = null; updateComposer(); });
action($("search-more"), () => search(true));

form("search", async (data) => {
  resetSearch();
  searchQuery = data.get("q").trim();
  if (searchQuery) await search();
});

form("compose", async (data) => {
  const { channel, thread, quote } = member;
  const body = data.get("body");
  const files = data.getAll("files").filter((file) => file.name);
  if (!body.trim() && !files.length) throw new Error("请输入消息内容或添加附件。");
  if (files.length > 8) throw new Error("每条消息最多包含 8 个附件。");
  const input = { client_id: key(), body };
  if (thread) input.root_seq = thread;
  if (quote) input.quote_seq = quote;
  input.mention_users = data.getAll("mention_user").map((value) => identifier(value));
  if (input.mention_users.length > 20) throw new Error("每条消息最多提醒 20 位成员。");
  await access(async (token) => {
    input.attachments = [];
    for (const file of files) input.attachments.push(await uploadFile(token, file));
    await json(token, messagePath(channel), "POST", input);
  });
  if (member.channel !== channel) return;
  $("compose").reset();
  $("mention-candidates").replaceChildren();
  member.quote = null;
  updateComposer();
  await loadTimeline();
  if (thread && member.thread === thread) await loadThread();
});

form("legacy-publication", async (data, operation) => {
  const channel = member.channel;
  const group = identifier(data.get("group_id"));
  const seq = identifier(data.get("source_seq"));
  const base = channelEndpoint(channel);
  if (operation === "preview") {
    $("legacy-preview").hidden = true;
    const preview = await access((token) => json(token, `${base}/publication-preview?group_id=${group}&source_seq=${seq}`));
    if (member.channel !== channel) return;
    $("legacy-preview").textContent = preview.body;
    $("legacy-preview").hidden = false;
    return;
  }
  if (!window.confirm(`将普通群 #${group} 的消息 #${seq} 发布到「${channel.title}」？`)) return;
  const receipt = await access((token) => json(token, `${base}/publications`, "POST", { client_id: key(), group_id: group, source_seq: seq }));
  if (member.channel !== channel) return;
  feedback(`正文已发布为频道消息 #${receipt.seq}。`);
  $("legacy-preview").hidden = true;
  await loadTimeline();
});
