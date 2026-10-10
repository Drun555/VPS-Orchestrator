const state = {
  servers: [],
  users: [],
  activeView: 'servers',
  editingServerID: ''
};

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

document.addEventListener('DOMContentLoaded', () => {
  $$('.tab').forEach(button => button.addEventListener('click', () => switchView(button.dataset.view)));
  $('#add-server').addEventListener('click', openNewServerDialog);
  $('#add-user').addEventListener('click', () => openUserDialog());
  $('#password-auth').addEventListener('change', togglePasswordFields);
  $('#server-form').addEventListener('submit', submitServer);
  $('#server-name-form').addEventListener('submit', submitServerName);
  $('#user-form').addEventListener('submit', submitUser);
  $('#user-form').elements.server_mode.addEventListener('change', () => { $('#user-server-selection').hidden = $('#user-form').elements.server_mode.value !== 'selected'; });
  $('#sync-all').addEventListener('click', syncAll);
  $('#delete-with-uninstall').addEventListener('click', () => performServerDelete('uninstall'));
  $('#delete-forget').addEventListener('click', () => performServerDelete('forget'));
  $('#job-close').addEventListener('click', () => $('#job-dialog').close());
  $$('.close-dialog').forEach(button => button.addEventListener('click', () => button.closest('dialog').close()));
  $('#settings-open').addEventListener('click', () => openSettings(false));
  $('#settings-form').addEventListener('submit', saveSettings);
  $('#login-form').addEventListener('submit', login);
  $('#resume-form').addEventListener('submit', resumeServer);
  $('#logout').addEventListener('click', async () => { await api('/api/logout', {method:'POST', body:'{}'}); location.reload(); });
  $('#login-dialog').addEventListener('cancel', event => event.preventDefault());
  $('#settings-dialog').addEventListener('cancel', event => { if (state.setupRequired) event.preventDefault(); });
  startSession();
});

function switchView(view) {
  state.activeView = view;
  $$('.tab').forEach(button => button.classList.toggle('active', button.dataset.view === view));
  $$('.view').forEach(section => section.classList.toggle('active', section.id === `${view}-view`));
}

async function api(path, options = {}) {
  const response = await fetch(path, { headers: { 'Content-Type': 'application/json', ...(options.headers || {}) }, ...options });
  const payload = response.status === 204 ? null : await response.json().catch(() => null);
  if (!response.ok) { if (response.status === 401 && !$('#login-dialog').open && !state.setupRequired) $('#login-dialog').showModal(); throw new Error(payload?.error || `HTTP ${response.status}`); }
  return payload;
}

async function loadOverview() {
  try {
    const overview = await api('/api/overview');
    state.servers = overview.servers || [];
    state.users = overview.users || [];
    $('#server-count').textContent = state.servers.length;
    $('#user-count').textContent = state.users.length;
    renderServers();
    renderUsers();
  } catch (error) {
    toast(`Не удалось загрузить данные: ${error.message}`);
  }
}

function renderServers() {
  const list = $('#servers-list');
  if (!state.servers.length) {
    list.innerHTML = '<div class="empty">Пока нет серверов. Добавьте чистую Ubuntu 22.04, 24.04 или 26.04 VPS.</div>';
    return;
  }
  list.innerHTML = state.servers.map(server => {
    const name = serverDisplayName(server);
    return `
    <article class="card">
      <div class="card-main">
        <div class="card-title"><strong>${escapeHTML(name)}</strong><span class="status ${statusClass(server.status)}">${statusLabel(server.status)}</span></div>
        <div class="meta">DuckDNS: ${escapeHTML(server.duckdns_url)}<br>SSH: ${escapeHTML(server.address)}<br>Host key: ${escapeHTML(server.ssh_host_fingerprint || 'не сохранён')}${server.last_error ? `<br><span class="danger-text">${escapeHTML(server.last_error)}</span>` : ''}</div>
      </div>
      <div class="card-actions">${server.provisioning ? `<button class="button secondary" data-resume-server="${server.id}">Продолжить установку</button>` : ''}<button class="icon-button" data-edit-server="${server.id}" title="Переименовать сервер">✎</button><button class="icon-button danger" data-delete-server="${server.id}" title="Удалить сервер">×</button></div>
    </article>`;
  }).join('');
  $$('[data-resume-server]', list).forEach(button => button.addEventListener('click', () => { $('#resume-form').dataset.serverId = button.dataset.resumeServer; $('#resume-form').reset(); $('#resume-dialog').showModal(); }));
  $$('[data-edit-server]', list).forEach(button => button.addEventListener('click', () => openServerNameDialog(button.dataset.editServer)));
  $$('[data-delete-server]', list).forEach(button => button.addEventListener('click', () => deleteServer(button.dataset.deleteServer)));
}

function renderUsers() {
  const list = $('#users-list');
  if (!state.users.length) {
    list.innerHTML = '<div class="empty">Пользователей пока нет. Создайте первый профиль доступа.</div>';
    return;
  }
  list.innerHTML = state.users.map(user => {
    const chips = state.servers.map(server => {
      const link = user.links?.[server.id];
      const allowed = user.server_mode !== 'selected' || (user.server_ids || []).includes(server.id);
      if (!allowed && !link) return '';

      return `<span class="link-chip ${link?.status || 'pending'}" title="${escapeHTML(link?.last_error || '')}">${escapeHTML(serverDisplayName(server))} · ${linkLabel(link)}</span>`;
    }).join('');
    return `<article class="card">
      <div class="card-main"><div class="card-title"><strong>${escapeHTML(user.email)}</strong></div><div class="meta">${escapeHTML(user.subscription_url)}</div><div class="link-statuses">${chips || '<span class="link-chip">Нет серверов</span>'}</div></div>
      <div class="card-actions"><button class="icon-button" data-edit-user="${user.id}" title="Изменить доступ">✎</button><button class="icon-button" data-retry-user="${user.id}" title="Повторить синхронизацию">↻</button><button class="icon-button" data-copy-user="${user.id}" title="Скопировать подписку">⧉</button><button class="icon-button danger" data-delete-user="${user.id}" title="Удалить пользователя">×</button></div>
    </article>`;
  }).join('');
  $$('[data-edit-user]', list).forEach(button => button.addEventListener('click', () => openUserDialog(button.dataset.editUser)));
  $$('[data-copy-user]', list).forEach(button => button.addEventListener('click', () => copySubscription(button.dataset.copyUser)));
  $$('[data-retry-user]', list).forEach(button => button.addEventListener('click', () => retryUser(button.dataset.retryUser)));
  $$('[data-delete-user]', list).forEach(button => button.addEventListener('click', () => deleteUser(button.dataset.deleteUser)));
}

function togglePasswordFields() {
  const enabled = $('#password-auth').checked;
  $('#password-fields').classList.toggle('visible', enabled);
  $('#server-form').elements.password.required = enabled;
  $('#key-fields').hidden = enabled;
  $('#server-form').elements.private_key_file.required = !enabled;
}

function openNewServerDialog() {
  const form = $('#server-form');
  form.reset();
  form.elements.private_key_file.required = true;
  $('#private-key-help').textContent = 'Ключ сохранится в master.json для последующего управления.';
  togglePasswordFields();
  $('#server-dialog').showModal();
}

async function submitServer(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const file = form.elements.private_key_file.files[0];
  try {
    const privateKey = file ? await file.text() : '';
    if (!privateKey && !form.elements.password_auth.checked) {
      toast('Выберите файл приватного SSH-ключа.');
      return;
    }
    const input = {
      display_name: form.elements.display_name.value,
      address: form.elements.address.value,
      private_key: form.elements.password_auth.checked ? '' : privateKey,
      passphrase: form.elements.passphrase.value,
      password_auth: form.elements.password_auth.checked,
      password: form.elements.password.value,
      duckdns_url: form.elements.duckdns_url.value,
      duckdns_token: form.elements.duckdns_token.value
    };
    const result = await api('/api/servers', { method: 'POST', body: JSON.stringify(input) });
    $('#server-dialog').close();
    form.reset(); togglePasswordFields();
    followJob(result.job_id, 'Установка VPS', {
      operation: 'provision',
    });
  } catch (error) { toast(error.message); }
}

function openServerNameDialog(id) {
  const server = state.servers.find(item => item.id === id);
  if (!server) return;
  state.editingServerID = id;
  const form = $('#server-name-form');
  form.reset();
  form.elements.display_name.value = server.display_name || '';
  $('#server-name-dialog').showModal();
  form.elements.display_name.focus();
}

async function submitServerName(event) {
  event.preventDefault();
  const id = state.editingServerID;
  if (!id) return;
  try {
    await api(`/api/servers/${id}`, {
      method: 'PATCH',
      body: JSON.stringify({ display_name: event.currentTarget.elements.display_name.value })
    });
    $('#server-name-dialog').close();
    state.editingServerID = '';
    await loadOverview();
    toast('Имя сервера сохранено.');
  } catch (error) { toast(error.message); }
}

function openUserDialog(id) {
  const user = state.users.find(item => item.id === id);
  const form = $('#user-form');
  form.reset();
  form.dataset.userId = user?.id || '';
  form.elements.email.value = user?.email || '';
  form.elements.email.disabled = !!user;
  form.elements.server_mode.value = user?.server_mode || 'all';
  $('#user-dialog-eyebrow').textContent = user ? 'ДОСТУП ПОЛЬЗОВАТЕЛЯ' : 'НОВЫЙ ПРОФИЛЬ';
  $('#user-dialog-title').textContent = user ? 'Изменить доступ' : 'Добавить пользователя';
  $('#user-server-options').innerHTML = state.servers.map(server => `<label class="server-choice"><input type="checkbox" name="server_ids" value="${escapeHTML(server.id)}" ${(user?.server_ids || []).includes(server.id) ? 'checked' : ''}><span>${escapeHTML(serverDisplayName(server))}</span></label>`).join('') || '<p>Серверы пока не добавлены.</p>';
  $('#user-server-selection').hidden = form.elements.server_mode.value !== 'selected';
  $('#user-dialog').showModal();
}

async function submitUser(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const id = form.dataset.userId;
  const payload = { server_mode: form.elements.server_mode.value, server_ids: $$('input[name="server_ids"]:checked', form).map(input => input.value) };
  if (!id) payload.email = form.elements.email.value;
  const button = form.querySelector('button[type="submit"]');
  button.disabled = true;
  try {
    const result = await api(id ? `/api/users/${id}` : '/api/users', { method: id ? 'PATCH' : 'POST', body: JSON.stringify(payload) });
    $('#user-dialog').close(); form.reset();
    followJob(result.job_id, id ? 'Изменение доступа' : 'Добавление пользователя');
  } catch (error) { toast(error.message); }
  finally { button.disabled = false; }
}

async function retryUser(id) {
  try {
    const result = await api(`/api/users/${id}/retry`, { method: 'POST', body: '{}' });
    followJob(result.job_id, 'Синхронизация пользователя');
  } catch (error) { toast(error.message); }
}

async function deleteUser(id) {
  const user = state.users.find(item => item.id === id);
  if (!user || !confirm(`Удалить пользователя ${user.email}? Subscription-ссылка перестанет работать сразу.`)) return;
  try {
    const result = await api(`/api/users/${id}`, { method: 'DELETE', body: '{}' });
    followJob(result.job_id, 'Удаление пользователя');
  } catch (error) { toast(error.message); }
}

async function deleteServer(id) {
  const server = state.servers.find(item => item.id === id);
  if (!server || !confirm(`Удалить сервер ${serverDisplayName(server)}?`)) return;
  $('#server-delete-dialog').dataset.serverId = id;
  $('#server-delete-dialog').showModal();
}

async function performServerDelete(mode) {
  const dialog = $('#server-delete-dialog');
  const id = dialog.dataset.serverId;
  dialog.close();
  if (!id) return;
  try {
    const result = await api(`/api/servers/${id}`, { method: 'DELETE', body: JSON.stringify({ mode }) });
    followJob(result.job_id, mode === 'uninstall' ? 'Удаление VPS' : 'Удаление записи');
  } catch (error) { toast(error.message); }
}

async function syncAll() {
  if (!confirm('Синхронизация пользователей удалит со VPS всех клиентов, которых нет в оркестраторе. Продолжить?')) return;
  try {
    const result = await api('/api/sync', { method: 'POST', body: '{}' });
    followJob(result.job_id, 'Синхронизация пользователей');
  } catch (error) { toast(error.message); }
}

async function copySubscription(id) {
  const user = state.users.find(item => item.id === id);
  if (!user) return;
  try {
    if (!navigator.clipboard?.writeText) throw new Error('Clipboard API unavailable');
    await navigator.clipboard.writeText(user.subscription_url);
    toast('Subscription-ссылка скопирована.');
  } catch (_) {
    $('#copy-fallback').value = user.subscription_url;
    $('#copy-dialog').showModal();
    $('#copy-fallback').focus(); $('#copy-fallback').select();
  }
}

function followJob(id, title, options = {}) {
  const dialog = $('#job-dialog');
  const log = $('#job-log');
  const status = $('#job-status');
  $('#job-title').textContent = title;
  if (options.reuseDialog && dialog.open) {
    log.textContent += `\n\n— ${title} —\n`;
  } else {
    log.textContent = '';
  }
  status.textContent = 'В процессе'; status.className = 'status running';
  setJobActionsRunning();
  if (!dialog.open) dialog.showModal();
  const source = new EventSource(`/api/jobs/${id}/events`);
  source.onmessage = event => {
    const item = JSON.parse(event.data);
    if (item.type === 'log' || item.type === 'warning') {
      log.textContent += `${item.type === 'warning' ? '⚠ ' : ''}${item.message}\n`;
      log.scrollTop = log.scrollHeight;
    }
    if (item.type === 'status') {
      source.close();
      finishJob(item.status, item.message, options);
    }
  };
  source.onerror = () => {
    if (!$('#job-close').disabled) return;
    source.close();
    pollJob(id, options);
  };
}

async function pollJob(id, options) {
  try {
    const job = await api(`/api/jobs/${id}`);
    $('#job-log').textContent = job.events.filter(event => event.message).map(event => `${event.type === 'warning' ? '⚠ ' : ''}${event.message}`).join('\n');
    $('#job-log').scrollTop = $('#job-log').scrollHeight;
    if (job.status === 'running') return setTimeout(() => pollJob(id, options), 1000);
    finishJob(job.status, job.error, options);
  } catch (error) {
    $('#job-close').disabled = false;
    toast(error.message);
  }
}

function setJobActionsRunning() {
  $('#job-close').disabled = true;
}

function finishJob(statusValue, message, options) {
  const failed = statusValue === 'error';
  $('#job-status').textContent = failed ? 'Error' : statusValue === 'success_with_warnings' ? 'Success · warning' : 'Success';
  $('#job-status').className = `status ${statusValue}`;
  if (failed && message) $('#job-log').textContent += `\nERROR: ${message}\n`;
  $('#job-close').disabled = false;

  loadOverview();
}

function statusClass(status) { return ['error', 'partial', 'success_with_warnings'].includes(status) ? status : ''; }
function statusLabel(status) { return ({ ready: 'Готов', partial: 'Частично', error: 'Ошибка', provisioning: 'Установка не завершена', setup_failed: 'Установка прервана' })[status] || status; }
function linkLabel(link) { return !link ? 'ожидает' : ({ ready: 'готов', error: 'ошибка', revoking: 'отзыв не завершён' })[link.status] || link.status; }
function serverDisplayName(server) { return server?.display_name?.trim() || server?.duckdns_url || server?.address || 'Сервер'; }
function escapeHTML(value = '') { const element = document.createElement('span'); element.textContent = value; return element.innerHTML; }
function toast(message) { const node = $('#toast'); node.textContent = message; node.classList.add('visible'); clearTimeout(toast.timer); toast.timer = setTimeout(() => node.classList.remove('visible'), 3500); }

async function startSession() {
  try {
    const session = await api('/api/session');
    state.setupRequired = session.setup_required;
    $('#logout').hidden = !session.password_enabled;
    if (session.setup_required) return openSettings(true);
    if (!session.authenticated) { $('#login-dialog').showModal(); return; }
    await loadOverview();
  } catch(error) { toast(error.message); }
}
async function openSettings(setup) {
  try {
    const settings = setup ? {} : await api('/api/settings');
    state.setupRequired = setup;
    const form = $('#settings-form'); form.reset();
    form.elements.base_url.value = settings.base_url || location.origin;
    form.elements.base_url.readOnly = !!settings.base_url_locked;
    form.elements.certbot_email.value = settings.certbot_email || '';
    form.elements.certbot_email.readOnly = !!settings.certbot_email_locked;
    form.elements.password.required = setup;
    form.elements.setup_token.required = setup;
    $('#setup-code-field').hidden = !setup; $('#setup-help').hidden = !setup; $('#settings-close').hidden = setup;
    $('#settings-title').textContent = setup ? 'Первый запуск' : 'Настройки';
    $('#settings-token-help').textContent = settings.duckdns_configured ? 'Токен сохранён. Оставьте поле пустым, чтобы сохранить его; новый токен применяется к будущим установкам.' : 'Общий токен для новых серверов. Его можно добавить позже.';
    $('#settings-error').textContent = '';
    $('#settings-dialog').showModal();
  } catch(error) { toast(error.message); }
}
async function saveSettings(event) {
  event.preventDefault(); const form = event.currentTarget;
  const input = {base_url:form.elements.base_url.value,certbot_email:form.elements.certbot_email.value,password:form.elements.password.value,setup_token:form.elements.setup_token.value};
  if (form.elements.duckdns_token.value) input.duckdns_token = form.elements.duckdns_token.value;
  const button = $('button[type="submit"]', form); button.disabled = true;
  try {
    const result = await api('/api/settings', {method:'PUT',body:JSON.stringify(input)});
    state.setupRequired = false; $('#settings-dialog').close(); form.reset();
    $('#logout').hidden = !result.password_enabled; await startSession(); toast('Настройки сохранены.');
  } catch(error) { $('#settings-error').textContent = error.message; }
  finally { button.disabled = false; }
}
async function login(event) {
  event.preventDefault();const form=event.currentTarget;
  try { await api('/api/login',{method:'POST',body:JSON.stringify({password:form.elements.password.value})}); form.reset(); $('#login-dialog').close(); await startSession(); }
  catch(error) { $('#login-error').textContent=error.message; }
}
async function resumeServer(event) {
  event.preventDefault();const form=event.currentTarget;
  const input = {password:form.elements.password.value}; if (form.elements.duckdns_token.value) input.duckdns_token = form.elements.duckdns_token.value;
  try { const result=await api(`/api/servers/${form.dataset.serverId}/resume`,{method:'POST',body:JSON.stringify(input)});form.reset();$('#resume-dialog').close();followJob(result.job_id,'Продолжение установки VPS'); }
  catch(error) { toast(error.message); }
}
