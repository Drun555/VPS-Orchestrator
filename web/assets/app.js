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
  initTraffic();
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
    state.trafficAuthenticated = true;
    refreshTraffic();
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
        <div class="card-title"><strong>${escapeHTML(name)}</strong>${server.private ? '<span class="private-badge">Приватный</span>' : ''}<span class="status ${statusClass(server.status)}">${statusLabel(server.status)}</span></div>
        <div class="meta">DuckDNS: ${escapeHTML(server.duckdns_url)}<br>SSH: ${escapeHTML(server.address)}<br>Шаблон: ${escapeHTML(siteTemplateLabel(server.site_template))}<br>Host key: ${escapeHTML(server.ssh_host_fingerprint || 'не сохранён')}${server.last_error ? `<br><span class="danger-text">${escapeHTML(server.last_error)}</span>` : ''}</div>
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
      const allowed = user.server_mode === 'selected' ? (user.server_ids || []).includes(server.id) : !server.private;
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
  $('#private-key-help').textContent = 'Ключ сохранится для управления сервером.';
  togglePasswordFields();
  $('#server-dialog').showModal();
  form.elements.display_name.focus();
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
      private: form.elements.private.checked,
      site_template: form.elements.site_template.value,
      display_name: form.elements.display_name.value,
      address: form.elements.address.value,
      private_key: form.elements.password_auth.checked ? '' : privateKey,
      passphrase: form.elements.passphrase.value,
      password_auth: form.elements.password_auth.checked,
      password: form.elements.password.value,
      duckdns_url: form.elements.duckdns_url.value
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
  $('#user-server-options').innerHTML = state.servers.map(server => `<label class="server-choice"><input type="checkbox" name="server_ids" value="${escapeHTML(server.id)}" ${(user?.server_ids || []).includes(server.id) ? 'checked' : ''}><span>${escapeHTML(serverDisplayName(server))}${server.private ? ' · приватный' : ''}</span></label>`).join('') || '<p>Серверы пока не добавлены.</p>';
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

function siteTemplateLabel(value) {
 return ({'file-hosting':'Selfhosted File Hosting',syncthing:'Private Syncthing Server','backup-vault':'Private Backup Vault','minecraft-worlds':'Worldkeep — Minecraft World Backups'})[value || 'file-hosting'] || value;
}


let trafficTimer;
let trafficLoading = false;
function initTraffic() {
  $('#traffic-period').addEventListener('change', () => { state.traffic = null; $('#traffic-table').innerHTML = '<div class="empty">Загружаем период…</div>'; refreshTraffic(); });
  $('#traffic-server').addEventListener('change', renderTraffic);
  $('#traffic-sync').addEventListener('click', syncAll);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) refreshTraffic(); });
}
async function refreshTraffic() {
  clearTimeout(trafficTimer);
  if (trafficLoading) return;
  if (document.hidden || !state.trafficAuthenticated || $('#login-dialog').open) {
    trafficTimer = setTimeout(refreshTraffic, 5000); return;
  }
  trafficLoading = true;
  const period = $('#traffic-period').value;
  try {
    const data = await api(`/api/traffic?period=${encodeURIComponent(period)}`, { signal: AbortSignal.timeout(10000) });
    if (period === $('#traffic-period').value) { state.traffic = data; renderTraffic(); }
    $('#traffic-error').hidden = true;
  } catch (error) {
    $('#traffic-error').textContent = `Статистика недоступна: ${error.message}. Показаны последние полученные данные.`;
    $('#traffic-error').hidden = false;
    // Old speed values must not masquerade as live observations.
    if (state.traffic) { state.traffic.rows.forEach(row => { row.rate = null; }); state.traffic.servers.forEach(server => { server.status = 'stale'; }); renderTraffic(); }
  } finally { trafficLoading = false; trafficTimer = setTimeout(refreshTraffic, period === $('#traffic-period').value ? 5000 : 0); }
}
function trafficSize(bytes) {
  const units = ['Б', 'КБ', 'МБ', 'ГБ', 'ТБ'];
  let i = 0; while (bytes >= 1000 && i < units.length - 1) { bytes /= 1000; i++; }
  return `${new Intl.NumberFormat('ru-RU', { maximumFractionDigits: i ? 2 : 0 }).format(bytes)} ${units[i]}`;
}
function trafficSpeed(bytes) { return `${trafficSize(bytes)}/с`; }
function renderTraffic() {
  const data = state.traffic; if (!data) return;
  const select = $('#traffic-server'); const selected = select.value;
  const servers = new Map(data.servers.map(server => [server.id, server.name]));
  data.rows.forEach(row => { if (!servers.has(row.server_id)) servers.set(row.server_id, `${row.server_name} · удалён`); });
  const options = '<option value="">Все серверы</option>' + [...servers].map(([id, name]) => `<option value="${escapeHTML(id)}">${escapeHTML(name)}</option>`).join('');
  if (select.innerHTML !== options) select.innerHTML = options;
  select.value = servers.has(selected) ? selected : '';
  const rows = data.rows.filter(row => !select.value || row.server_id === select.value);
  const health = data.servers.filter(server => !select.value || server.id === select.value);
  const labels = { ready: 'Обновляется', warming: 'Измеряем скорость…', waiting: 'Ожидаем измерения', disabled: 'Нужна синхронизация', offline: 'Нет связи / ошибка SSH', stale: 'Нет свежих данных', provisioning: 'Установка не завершена' };
  $('#traffic-health').innerHTML = health.map(server => `<span class="link-chip ${server.status === 'ready' ? 'ready' : 'error'}" title="${server.updated_at && !server.updated_at.startsWith('0001') ? 'Последнее измерение: ' + escapeHTML(new Date(server.updated_at).toLocaleString('ru-RU')) : 'Измерений пока нет'}">${escapeHTML(server.name)} · ${labels[server.status] || 'Нет данных'}</span>`).join('');
  $('#traffic-enable').hidden = !data.servers.some(server => server.status === 'disabled');
  const upload = rows.reduce((n, row) => n + row.upload, 0), download = rows.reduce((n, row) => n + row.download, 0);
  const complete = health.length > 0 && health.every(server => server.status === 'ready');
  const measured = health.some(server => server.status === 'ready');
  const upRate = rows.reduce((n, row) => n + (row.rate?.upload || 0), 0), downRate = rows.reduce((n, row) => n + (row.rate?.download || 0), 0);
  const active = new Set(rows.filter(row => row.rate && row.rate.upload + row.rate.download > 0).map(row => row.user_id)).size;
  $('#traffic-summary').innerHTML = `<article><span>Трафик за период</span><strong>${trafficSize(upload + download)}</strong><small>↓ ${trafficSize(download)} получено · ↑ ${trafficSize(upload)} отправлено</small></article><article><span>Скорость сейчас ${complete ? '' : '· неполные данные'}</span><strong>↓ ${measured ? trafficSpeed(downRate) : '—'}</strong><small>↑ ${measured ? trafficSpeed(upRate) : '—'} отправка</small></article><article><span>Передают данные сейчас</span><strong>${measured ? active : '—'}<em> пользователей</em></strong><small>${complete ? 'По всем выбранным серверам' : 'Только по доступным измерениям'}</small></article>`;
  const groups = new Map();
  rows.forEach(row => {
    if (!groups.has(row.user_id)) groups.set(row.user_id, { name: row.user_name, rows: [], upload: 0, download: 0, up: 0, down: 0, measured: true });
    const group = groups.get(row.user_id); group.rows.push(row); group.upload += row.upload; group.download += row.download;
    group.up += row.rate?.upload || 0; group.down += row.rate?.download || 0; if (!row.rate) group.measured = false;
  });
  const header = '<div class="traffic-columns traffic-column-head"><span>Пользователь / сервер</span><span>↓ Получает сейчас</span><span>↑ Отправляет сейчас</span><span>Трафик за период</span></div>';
  const content = [...groups.values()].sort((a, b) => (b.up + b.down) - (a.up + a.down) || (b.upload + b.download) - (a.upload + a.download) || a.name.localeCompare(b.name)).map(group => {
    const active = group.up + group.down > 0;
    const rate = (value) => group.measured ? trafficSpeed(value) : value ? `${trafficSpeed(value)} *` : '—';
    return `<details class="traffic-user"><summary class="traffic-columns"><span><strong>${escapeHTML(group.name)}</strong><small class="${active ? 'traffic-active' : ''}">${active ? '● Передаёт данные' : group.measured ? 'Нет передачи' : 'Ожидаем данные'} · ${group.rows.length} сервер(а)</small></span><span>${rate(group.down)}</span><span>${rate(group.up)}</span><span><strong>${trafficSize(group.upload + group.download)}</strong><small>↓ ${trafficSize(group.download)} · ↑ ${trafficSize(group.upload)}</small></span></summary><div class="traffic-detail">${group.rows.sort((a, b) => a.server_name.localeCompare(b.server_name)).map(row => `<div class="traffic-columns"><span>${escapeHTML(row.server_name)}</span><span>${row.rate ? trafficSpeed(row.rate.download) : '—'}</span><span>${row.rate ? trafficSpeed(row.rate.upload) : '—'}</span><span>${trafficSize(row.upload + row.download)}<small>↓ ${trafficSize(row.download)} · ↑ ${trafficSize(row.upload)}</small></span></div>`).join('')}</div></details>`;
  }).join('');
  // Preserve expanded user rows across polling without losing keyboard focus.
  const table = $('#traffic-table');
  const expanded = new Set([...table.querySelectorAll('details[open]')].map(el => el.querySelector('strong').textContent));
  const focused = table.contains(document.activeElement) ? document.activeElement.closest('details')?.querySelector('strong')?.textContent : null;
  table.innerHTML = content ? header + content : '<div class="empty">Пока нет данных за выбранный период.</div>';
  table.querySelectorAll('details').forEach(el => { const name = el.querySelector('strong').textContent; el.open = expanded.has(name); if (name === focused) el.querySelector('summary').focus({preventScroll: true}); });
  const since = new Date(data.started_at).toLocaleString('ru-RU');
  $('#traffic-note').textContent = `Учёт с ${since}. Обновление ~5 с в открытой панели, фоновый сбор — раз в минуту. Объём — загрузка + отдача через VPN; 1 ГБ = 1 000 000 000 Б. Направления указаны со стороны пользователя. История хранится 62 дня; месяцы — по UTC. ${data.gaps ? 'Были сбросы счётчиков Xray: итог может не включать трафик между последним опросом и перезапуском. ' : ''}— означает отсутствие свежего измерения, * — частичную скорость. ${data.save_error || ''}`;
}
