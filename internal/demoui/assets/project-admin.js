import { $, absoluteDate, action, displayDate, element, feedback, form, identifier, json, key, list, localDate, request, rowButton, saveResponse } from "./project-common.js";

const admin = { token: "", links: [], project: null, archive: null, preview: null };
const states = { active: "有效", frozen: "冻结", closed: "已关闭", pending: "待签认", expired: "已到期" };
const archiveStates = { queued: "待生成", ready: "已生成", failed: "生成失败", invalidated: "凭证已失效", expired: "已到期" };
const events = {
  accepted: "确认合作", active: "解除本组织冻结", frozen: "冻结合作", closed: "关闭合作",
  grant_set: "设置成员授权", grant_revoked: "撤回成员授权", message_hidden: "删除消息",
  attachment_download_authorized: "授权跨组织附件下载", export_requested: "申请归档", export_ready: "归档已生成",
  export_retried: "重试归档", export_download_authorized: "授权归档下载",
  legacy_message_published: "发布普通群消息",
};

function endpoint(project = admin.project) {
  return `/v1/admin/partner-links/${project.link_id}`;
}

function selectLabel(label, value) {
  const option = element("option", label);
  option.value = value;
  return option;
}

function renderAdminLinks() {
  list($("admin-links"), admin.links.map((project) => rowButton(
    `${project.space_title} · ${states[project.expired ? "expired" : project.state]} · #${project.link_id}`,
    project.link_id === admin.project?.link_id, () => selectProject(project),
  )), "签认后会在这里显示合作关系。");
}

function invitationCard(invite) {
  const row = element("article", "");
  row.append(element("h3", `${invite.title} · ${states[invite.status]}`));
  row.append(element("p", invite.purpose));
  row.append(element("small", `${invite.host_tenant_key}/${invite.host_app_key} · 席位 ${invite.seat_limit} · 合作到期 ${displayDate(invite.expires_at)}`));
  row.append(element("small", invite.retention_days ? `消息保留 ${invite.retention_days} 天` : "按合作期限控制访问"));
  if (invite.status !== "pending") return row;
  row.append(element("small", `签认截止 ${displayDate(invite.accept_before)} · 政策修订 ${invite.policy_rev}`));
  const write = element("input", "");
  write.type = "checkbox";
  write.checked = invite.scope.includes("thread:write");
  write.disabled = !write.checked;
  const writeLabel = element("label", "", "check");
  writeLabel.append(write, document.createTextNode("接受消息和线程写入权限"));
  const history = element("input", "");
  history.type = "checkbox";
  history.disabled = !invite.share_history;
  const historyLabel = element("label", "", "check");
  historyLabel.append(history, document.createTextNode(invite.share_history ? "同意共享加入前的历史" : "邀请仅允许读取加入后的历史"));
  row.append(writeLabel, historyLabel, rowButton("签认邀请", undefined, async () => {
    const input = { invitation_id: invite.invitation_id, policy_rev: invite.policy_rev, scope: ["project:read"], share_history: history.checked, retention_days: invite.retention_days };
    if (write.checked) input.scope.push("thread:write");
    const path = `/shared-spaces/${invite.space_id}/partners/${invite.peer.tenant_id}/accept`;
    const relation = await json(admin.token, path, "POST", input, { "Idempotency-Key": key() });
    await refreshAdmin(relation.link_id);
    feedback(`已确认合作关系 #${relation.link_id}。`);
  }));
  return row;
}

async function refreshAdmin(selected = admin.project?.link_id) {
  const token = admin.token;
  const [spaces, inbox, links] = await Promise.all([
    json(token, "/v1/admin/shared-spaces"), json(token, "/v1/admin/partner-invitations"), json(token, "/v1/admin/partner-links"),
  ]);
  if (token !== admin.token) return;
  const currentSpace = $("invite-spaces").value;
  $("invite-spaces").replaceChildren(...spaces.items.map((space) => selectLabel(`${space.title} · #${space.space_id}`, space.space_id)));
  if (spaces.items.some((space) => String(space.space_id) === currentSpace)) $("invite-spaces").value = currentSpace;
  list($("admin-invitations"), inbox.items.map(invitationCard), "暂未收到合作邀请。");
  admin.links = links.items;
  const project = admin.links.find((link) => link.link_id === selected);
  if (project) {
    await selectProject(project);
  } else {
    admin.project = null;
    admin.archive = null;
    admin.preview = null;
    renderAdminLinks();
    $("admin-project").hidden = true;
  }
}

async function selectProject(project) {
  if (admin.project?.link_id !== project.link_id) {
    admin.archive = null;
    admin.preview = null;
    $("grant-proof").hidden = true;
    $("change-grant").reset();
  }
  admin.project = project;
  $("admin-project").hidden = false;
  $("admin-heading").textContent = project.space_title;
  renderAdminLinks();
  renderProjectState();
  renderArchive();
  await Promise.all([loadGrants(), loadAudit()]);
}

function renderProjectState() {
  const project = admin.project;
  const status = states[project.expired && project.state !== "closed" ? "expired" : project.state];
  const retention = project.retention_days ? ` · 保留 ${project.retention_days} 天` : "";
  $("admin-state").textContent = `${project.host_tenant}/${project.host_app} ↔ ${project.peer_tenant}/${project.peer_app} · ${status} · 授权代际 ${project.grant_epoch}${retention}`;
  $("channel-controls").hidden = project.my_side !== "host" || project.state !== "active" || project.expired;
  $("change-grant").hidden = project.state === "closed";
  $("preview-export").disabled = project.state !== "active" || project.expired;
  if ($("preview-export").disabled || admin.preview?.epoch !== project.grant_epoch) admin.preview = null;
  renderExportPreview();
  renderChannelPreview();
  $("change-grant").elements.expiry.value ||= localDate(project.expires_at);
  const buttons = $("state-actions");
  buttons.replaceChildren();
  if (project.state === "closed") return;
  const ownFrozen = project.my_side === "host" ? project.host_frozen : project.peer_frozen;
  if (!project.expired) buttons.append(rowButton(ownFrozen ? "解除本方冻结" : "冻结合作", undefined,
    () => changeState(project, ownFrozen ? "active" : "frozen")));
  const close = rowButton("关闭合作", undefined, async () => {
    if (window.confirm("确认关闭合作关系？双方将失去在线读取权限。")) await changeState(project, "closed");
  });
  close.classList.add("danger");
  buttons.append(close);
}

async function changeState(project, state) {
  const receipt = await json(admin.token, `${endpoint(project)}/state`, "PUT", { state, grant_epoch: project.grant_epoch });
  await refreshAdmin(project.link_id);
  if (admin.archive) await refreshArchive();
  feedback(state === "closed" ? `合作已关闭 · 关闭代际 ${receipt.closure_epoch}。可查看审计和归档回执。` : "合作状态已更新。");
}

async function loadGrants() {
  const project = admin.project;
  const { items } = await json(admin.token, `${endpoint(project)}/grants`);
  if (admin.project !== project) return;
  const active = items.filter((grant) => grant.active).length;
  const capacity = project.my_side === "host" ? "主持方" : `合作方席位上限 ${project.seat_limit}`;
  const rows = items.map((grant) => rowButton(
    `${grant.display_name} · 用户 ${grant.user_id} · ${grant.active ? "有效" : "失效"} · v${grant.grant_version}`, undefined, () => {
      const fields = $("change-grant").elements;
      fields.user.value = grant.user_id;
      fields.version.value = grant.grant_version;
      fields.role.value = grant.duty_role;
      fields.expiry.value = localDate(grant.expires_at);
    },
  ));
  rows.unshift(element("p", `当前列表有效 ${active} 项 · ${capacity}`, "muted"));
  $("admin-grants").replaceChildren(...rows);
}

async function loadAudit() {
  const project = admin.project;
  const { items } = await json(admin.token, `${endpoint(project)}/audit`);
  if (admin.project !== project) return;
  list($("audit"), items.slice().reverse().map((event) => {
    const row = element("li", "");
    row.append(element("p", `${events[event.action] || event.action} · 授权代际 ${event.grant_epoch}`),
      element("small", `#${event.id} · 组织 ${event.actor.tenant_id} · ${displayDate(event.occurred_at)}`));
    if (event.detail) {
      const details = element("details", "");
      details.append(element("summary", "业务回执"), element("code", JSON.stringify(event.detail)));
      row.append(details);
    }
    return row;
  }), "还没有审计记录。");
}

function renderArchive() {
  const archive = admin.archive;
  const proof = $("export-proof");
  proof.hidden = !archive;
  $("refresh-export").hidden = !archive;
  $("download-export").hidden = archive?.state !== "ready";
  $("export-next").hidden = archive?.state !== "ready" || !archive?.has_more;
  $("retry-export").hidden = archive?.state !== "failed";
  if (!archive) return;
  proof.replaceChildren(element("p", `归档 #${archive.export_id} · ${archiveStates[archive.state] || archive.state} · ${archive.item_count} 条消息`));
  if (archive.archive_digest) proof.append(element("code", `SHA-256 ${archive.archive_digest}`));
  if (archive.last_error) proof.append(element("p", archive.last_error));
}

function renderExportPreview() {
  const view = $("export-preview");
  view.hidden = !admin.preview;
  $("request-export").hidden = !admin.preview;
  view.textContent = admin.preview ? `本组织有 ${admin.preview.item_count} 条可归档消息 · 截至项目序号 ${admin.preview.snapshot_project_seq}` : "";
}

async function previewArchive() {
  const project = admin.project;
  const preview = await json(admin.token, `${endpoint(project)}/export-preview`);
  if (admin.project !== project) return;
  admin.preview = { ...preview, epoch: project.grant_epoch };
  renderExportPreview();
}

async function requestArchive(continuation = false) {
  const project = admin.project;
  const input = continuation ? { after_project_seq: admin.archive.through_project_seq, snapshot_project_seq: admin.archive.snapshot_project_seq }
    : { snapshot_project_seq: admin.preview.snapshot_project_seq };
  const archive = await json(admin.token, `${endpoint(project)}/exports`, "POST", input, { "Idempotency-Key": key() });
  if (admin.project !== project) return;
  admin.archive = archive;
  admin.preview = null;
  renderExportPreview();
  renderArchive();
  feedback("归档请求已受理。生成后可刷新回执并下载文件。");
  await loadAudit();
}

async function refreshArchive() {
  const project = admin.project;
  const id = admin.archive.export_id;
  const archive = await json(admin.token, `${endpoint(project)}/exports/${id}`);
  if (admin.project !== project || admin.archive?.export_id !== id) return;
  admin.archive = archive;
  renderArchive();
}

form("admin-login", async (data) => {
  admin.token = data.get("token").trim();
  try {
    await refreshAdmin();
  } catch (error) {
    admin.token = "";
    throw error;
  }
  $("admin-login").reset();
  $("admin-login").hidden = true;
  $("admin-workspace").hidden = false;
});

action($("admin-logout"), () => {
  Object.assign(admin, { token: "", links: [], project: null, archive: null, preview: null });
  $("admin-workspace").hidden = true;
  $("admin-login").hidden = false;
  $("grant-proof").hidden = true;
});
action($("admin-refresh"), async () => { await refreshAdmin(); if (admin.archive) await refreshArchive(); });
action($("load-audit"), loadAudit);
action($("preview-export"), previewArchive);
action($("request-export"), () => requestArchive());
action($("export-next"), () => requestArchive(true));
action($("refresh-export"), refreshArchive);
action($("retry-export"), async () => {
  await request(admin.token, `${endpoint()}/exports/${admin.archive.export_id}/retry`, { method: "POST" });
  await refreshArchive();
});
action($("download-export"), async () => {
  const token = admin.token;
  const archive = admin.archive;
  const path = `${endpoint()}/exports/${archive.export_id}`;
  try {
    const pass = await json(token, `${path}/grant`, "POST");
    const response = await request(token, `${path}/content`, { headers: { "X-Export-Token": pass.token } });
    if (response.headers.get("X-Archive-SHA256") !== archive.archive_digest) throw new Error("归档摘要与回执不一致。");
    if (admin.token === token) await saveResponse(response, `project-archive-${archive.export_id}.json`);
  } finally {
    if (admin.token === token && admin.archive?.export_id === archive.export_id) await refreshArchive();
  }
});

form("create-space", async (data) => {
  const space = await json(admin.token, "/v1/admin/shared-spaces", "POST", Object.fromEntries(data));
  $("create-space").reset();
  await refreshAdmin();
  $("invite-spaces").value = String(space.space_id);
  feedback(`空间 #${space.space_id} 已创建。`);
});

const inviteForm = $("invite");
inviteForm.elements.accept_before.value = localDate(Date.now() + 24 * 60 * 60 * 1000);
inviteForm.elements.expires_at.value = localDate(Date.now() + 30 * 24 * 60 * 60 * 1000);
form("invite", async (data) => {
  const space = identifier(data.get("space"));
  const input = { peer_tenant: data.get("peer_tenant"), peer_app: data.get("peer_app"), seat_limit: identifier(data.get("seat_limit")),
    scope: ["project:read"], share_history: data.has("history"), retention_days: identifier(data.get("retention_days"), true),
    accept_before: absoluteDate(data.get("accept_before")), expires_at: absoluteDate(data.get("expires_at")) };
  if (data.has("thread")) input.scope.push("thread:write");
  const invite = await json(admin.token, `/v1/admin/shared-spaces/${space}/invitations`, "POST", input, { "Idempotency-Key": key() });
  feedback(`邀请 #${invite.invitation_id} 已发给 ${input.peer_tenant}/${input.peer_app}。`);
});

function renderChannelPreview() {
  const fields = $("create-channel").elements;
  const audience = fields.audience.value === "host" ? "合作组织看不到该频道。"
    : (fields.history_from.value === "join" ? "新访客只能看到加入后的记录。" : "新访客能看到频道历史，需要双方已签认共享历史。");
  $("channel-preview").textContent = audience;
}

$("create-channel").addEventListener("change", renderChannelPreview);
form("create-channel", async (data) => {
  const result = await json(admin.token, `${endpoint()}/channels`, "POST", Object.fromEntries(data));
  $("create-channel").reset();
  renderChannelPreview();
  feedback(`频道 #${result.channel_id} 已创建。`);
});

form("change-grant", async (data, operation) => {
  const project = admin.project;
  const input = { user_id: identifier(data.get("user")), expected_version: identifier(data.get("version"), true), revoke: operation === "revoke" };
  if (!input.revoke) {
    input.duty_role = data.get("role");
    input.expires_at = absoluteDate(data.get("expiry"));
  }
  const receipt = await json(admin.token, `${endpoint(project)}/grants`, "POST", input, { "Idempotency-Key": key() });
  await refreshAdmin(project.link_id);
  const proof = $("grant-proof");
  proof.hidden = false;
  proof.replaceChildren(element("p", `用户 ${receipt.user_id} · ${receipt.revoked ? "已撤权" : "已授权"} · 版本 ${receipt.grant_version} · 授权代际 ${receipt.grant_epoch} · 有效席位 ${receipt.active_seats}`));
  for (const device of receipt.device_authorizations || []) {
    proof.append(element("p", `设备 #${device.device_id} · 上次成功鉴权代际 ${device.grant_epoch} · ${displayDate(device.authorized_at)}`));
  }
  if (receipt.more_device_authorizations) proof.append(element("p", "本次回执显示最近的 100 台设备。"));
  $("change-grant").elements.version.value = receipt.grant_version;
  if (admin.archive) await refreshArchive();
});
