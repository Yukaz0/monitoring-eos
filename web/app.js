const API_BASE = (window.location.protocol + '//' + window.location.hostname + ':5118');
let latestReportText = '';
let latestReportId = null;
let refreshInFlight = false;
let reportPolling = false;
let reportPollTimer = null;
let reportPollBaseId = null;
let reportPollRequestInFlight = false;
let autoRefreshTimer = null;

function getApiKey() {
  // Tidak ada kunci default yang di-hardcode: kunci diisi operator sekali lewat
  // kolom di dashboard lalu tersimpan di localStorage browser. Menaruh nilai
  // asli di repo akan membocorkan kunci API ke siapa pun yang membaca repo.
  return localStorage.getItem('mon_api_key') || '';
}
function setApiKey(v) {
  localStorage.setItem('mon_api_key', v || '');
}

function escapeHtml(value) {
  return String(value == null ? '' : value)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#039;');
}

function firstValue(obj, names) {
  if (!obj) return undefined;
  for (let i = 0; i < names.length; i++) {
    if (Object.prototype.hasOwnProperty.call(obj, names[i]) && obj[names[i]] != null) {
      return obj[names[i]];
    }
  }
  return undefined;
}

function recordId(record) {
  return firstValue(record, ['ID', 'id']);
}

function reportContent(record) {
  return firstValue(record, ['Content', 'content']);
}

function reportGeneratedAt(record) {
  return firstValue(record, ['GeneratedAt', 'generated_at']);
}

function scheduleName(sc) {
  return firstValue(sc, ['Name', 'name']) || '';
}

function scheduleCron(sc) {
  return firstValue(sc, ['CronExpr', 'cron_expr']) || '';
}

function scheduleActive(sc) {
  return !!firstValue(sc, ['Active', 'active']);
}

function headers(extra) {
  const h = Object.assign({ 'X-API-Key': getApiKey() }, extra || {});
  return h;
}

function toast(msg, isErr) {
  const box = document.getElementById('toast-box');
  const el = document.createElement('div');
  el.className = 'toast' + (isErr ? ' toast-err' : '');
  el.textContent = msg;
  box.appendChild(el);
  setTimeout(function () { el.remove(); }, 4000);
}

function setBadge(id, text, ok) {
  const el = document.getElementById(id);
  if (!el) return;
  const dot = el.querySelector('.status-dot');
  if (dot) {
    el.replaceChildren(dot, document.createTextNode(text));
  } else {
    el.textContent = text;
  }
  el.classList.toggle('ok', !!ok);
  el.classList.toggle('bad', !ok);
}

async function apiFetch(path, opts) {
  const rawOpts = opts || {};
  const o = Object.assign({}, rawOpts);
  o.headers = headers(rawOpts.headers);
  const resp = await fetch(API_BASE + path, o);
  if (!resp.ok) {
    let detail = 'HTTP ' + resp.status;
    const body = await resp.text();
    try {
      const j = JSON.parse(body);
      if (j.error) detail = j.error;
    } catch (e) {
      if (body) detail = body.trim();
    }
    const err = new Error(detail);
    err.status = resp.status;
    throw err;
  }
  return resp;
}

async function apiJSON(path, opts) {
  const resp = await apiFetch(path, opts);
  return resp.json();
}
// Render status server dari laporan terbaru. Content dari report/latest
// adalah teks laporan; angka di-parsing dari format "Label: value".
function parseLatest(text) {
  const ambil = (label) => {
    const re = new RegExp('\\* ' + label + ':\\s*([0-9.]+)');
    const m = text.match(re);
    return m ? m[1] : null;
  };
  const uptime = ambil('Uptime');
  return {
    uptime: uptime,
    cpu: ambil('CPU Busy'),
    load: ambil('Sys Load'),
    ram: ambil('RAM Used'),
    swap: ambil('SWAP Used'),
    kontainer: (text.match(/\* Service Container Running:\s*(\d+)/) || [])[1] || null
  };
}

function renderStatusServer(r) {
  const grid = document.getElementById('grid-status');
  if (!r) {
    grid.innerHTML = '<p class="empty-state">Belum ada laporan tersedia. Laporan akan muncul setelah siklus backend selesai.</p>';
    return;
  }
  const p = parseLatest(r.Content || '');
  const kartu = [
    ['Uptime', p.uptime ? p.uptime + ' hari' : '-'],
    ['CPU Busy', p.cpu ? p.cpu + '%' : '-'],
    ['Sys Load', p.load ? p.load + '%' : '-'],
    ['RAM Used', p.ram ? p.ram + '%' : '-'],
    ['SWAP Used', p.swap ? p.swap + '%' : '-'],
    ['Container Running', p.kontainer || '-']
  ];
  grid.innerHTML = kartu.map(function (k) {
    return '<div class="kartu"><div class="label">' + escapeHtml(k[0]) + '</div><div class="nilai">' + escapeHtml(k[1]) + '</div></div>';
  }).join('');
}

function renderTelemetri(r) {
  const aktif = [],
        nodata = [];
  if (r && r.Content) {
    const bagian = r.Content.split('[ STATUS TELEMETRI RTU ]')[1] || '';
    let target = null;
    bagian.split('\n').forEach(function (baris) {
      if (baris.indexOf('Lambung Active') >= 0) { target = aktif; return; }
      if (baris.indexOf('no data from broker') >= 0) { target = nodata; return; }
      if (target !== null && baris.indexOf('* ') === 0) {
        target.push(baris.slice(2).trim());
      }
    });
  }
  document.getElementById('n-active').textContent = aktif.length;
  document.getElementById('n-nodata').textContent = nodata.length;
  document.getElementById('list-active').innerHTML =
    aktif.length ? aktif.map(li).join('') : '<li>(tidak ada data)</li>';
  document.getElementById('list-nodata').innerHTML =
    nodata.length ? nodata.map(li).join('') : '<li>(tidak ada data)</li>';
}

function li(s) { return '<li>' + escapeHtml(s) + '</li>'; }

function fmtWaktu(iso) {
  try { return new Date(iso).toLocaleString('id-ID'); } catch (e) { return iso; }
}

function setReportState(text, state) {
  const stateEl = document.getElementById('report-state');
  stateEl.className = 'state-label';
  if (state) stateEl.classList.add('is-' + state);
  stateEl.textContent = text;
}

function setReportProgressVisible(visible) {
  const wrap = document.getElementById('report-progress-wrap');
  if (wrap) wrap.hidden = !visible;
}

function updateReportProgress(status) {
  const progress = document.getElementById('report-progress');
  const note = document.getElementById('report-progress-text');
  const fase = status && status.fase;
  const expected = Number(status && status.rtu_diharapkan) || 0;
  const percentage = status && status.persen != null ? Number(status.persen) : null;
  const collected = Number(status && status.rtu_terkumpul) || 0;
  const elapsed = Number(status && status.berjalan_detik) || 0;
  const remaining = Number(status && status.perkiraan_sisa_detik) || 0;

  setReportProgressVisible(true);
  if (fase === 'mengumpulkan_traffic' && expected > 0 && Number.isFinite(percentage)) {
    progress.max = 100;
    progress.value = Math.min(Math.max(percentage, 0), 99);
    note.textContent = 'Menunggu traffic RTU: ' + collected + '/' + expected +
      ' • berjalan ' + elapsed + 's • perkiraan sisa ' + remaining + 's';
    setReportState('SEDANG DIBUAT', 'running');
    return;
  }

  progress.removeAttribute('value');
  if (fase === 'menyusun_dan_mengirim') {
    note.textContent = 'Menyusun & mengirim laporan…';
    setReportState('SEDANG DIBUAT — MENYUSUN & MENGIRIM', 'running');
    return;
  }

  note.textContent = 'Menunggu data portal…';
  setReportState('SEDANG DIBUAT — MENUNGGU DATA PORTAL', 'running');
}

function setRunButtonRunning(running) {
  const button = document.getElementById('btn-refresh-report');
  const label = button && button.querySelector('span:not(.btn-symbol)');
  if (!button) return;
  button.disabled = !!running;
  button.classList.toggle('is-loading', !!running);
  if (running) {
    button.setAttribute('aria-busy', 'true');
  } else {
    button.removeAttribute('aria-busy');
  }
  if (label) label.textContent = running ? 'Membuat' : 'Buat laporan';
}

function renderRiwayat(list) {
  const tb = document.getElementById('tabel-riwayat');
  const data = (list || []).slice(0, 10);
  if (!data.length) {
    tb.innerHTML = '<tr><td colspan="3">Belum ada laporan tersedia.</td></tr>';
    return;
  }
  tb.innerHTML = data.map(function (rep) {
    const content = reportContent(rep) || '';
    const generatedAt = reportGeneratedAt(rep);
    const pot = content.split('\n').slice(0, 2).join(' - ');
    return '<tr data-id="' + escapeHtml(recordId(rep)) + '"><td>' + escapeHtml(fmtWaktu(generatedAt)) +
      '</td><td>' + escapeHtml(pot) + '</td><td><span class="detail-mark" aria-hidden="true">›</span><span class="sr-only">Buka detail</span></td></tr>';
  }).join('');
  tb.querySelectorAll('tr').forEach(function (tr) {
    tr.addEventListener('click', function () {
      const rep = data.filter(function (x) { return String(recordId(x)) === tr.dataset.id; })[0];
      if (!rep) return;
      const modal = document.getElementById('modal');
      const generatedAt = reportGeneratedAt(rep);
      document.getElementById('modal-judul').textContent = 'Laporan ID ' + recordId(rep) + ' - ' + fmtWaktu(generatedAt);
      document.getElementById('modal-pre').textContent = reportContent(rep) || '';
      modal.classList.remove('hidden');
    });
  });
}

function renderDailyReport(r) {
  const report = reportContent(r);
  const reportEl = document.getElementById('daily-report');
  const generatedEl = document.getElementById('report-generated-at');
  const copyButton = document.getElementById('btn-copy-report');

  latestReportId = recordId(r) || null;
  if (!report) {
    latestReportText = '';
    reportEl.textContent = 'Belum ada daily report. Report akan muncul setelah siklus collector selesai.';
    reportEl.classList.add('is-empty');
    if (!reportPolling) setReportState('BELUM ADA LAPORAN', 'empty');
    generatedEl.textContent = 'Belum ada laporan';
    copyButton.disabled = true;
    return;
  }

  latestReportText = report;
  reportEl.textContent = report;
  reportEl.classList.remove('is-empty');
  if (!reportPolling) {
    setReportState('SIAP DISALIN', 'ready');
    setReportProgressVisible(false);
  }
  generatedEl.textContent = fmtWaktu(reportGeneratedAt(r));
  copyButton.disabled = false;
}

function fallbackCopyText(text) {
  const helper = document.createElement('textarea');
  helper.className = 'clipboard-helper';
  helper.value = text;
  document.body.appendChild(helper);
  helper.focus();
  helper.select();
  let copied = false;
  try {
    copied = document.execCommand('copy');
  } catch (e) {
    copied = false;
  }
  helper.remove();
  return copied;
}

async function copyDailyReport() {
  if (!latestReportText) {
    toast('Belum ada daily report untuk disalin', true);
    return;
  }

  let copied = false;
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(latestReportText);
      copied = true;
    } else {
      copied = fallbackCopyText(latestReportText);
    }
  } catch (e) {
    copied = fallbackCopyText(latestReportText);
  }

  if (!copied) {
    toast('Clipboard tidak tersedia di browser ini', true);
    return;
  }

  const label = document.querySelector('#btn-copy-report span:not(.btn-symbol)');
  if (label) {
    label.textContent = 'Tersalin';
    setTimeout(function () { label.textContent = 'Salin laporan'; }, 1800);
  }
  toast('Daily report disalin ke clipboard');
}
async function muatHeaderStatus() {
  try {
    const h = await apiJSON('/api/health');
    setBadge('badge-health', 'API: ' + (h.status || 'ok'), true);
  } catch (e) {
    setBadge('badge-health', 'API: tidak terhubung', false);
  }
  try {
    const wa = await apiJSON('/api/wa/status');
    renderWaBadge(wa);
  } catch (e) {
    setBadge('badge-wa', 'WA: tidak terhubung', false);
  }
}

function renderWaBadge(wa) {
  const st = wa && wa.status ? wa.status : 'unknown';
  setBadge('badge-wa', 'WA: ' + st, st === 'connected');
  setBadge('badge-wa-koneksi', st, st === 'connected');
  document.getElementById('btn-qr').style.display = wa && wa.hasQR ? '' : 'none';
}

async function muatLaporan() {
  let latest = null;
  try {
    latest = await apiJSON('/api/report/latest');
  } catch (e) {
    if (String(e.message).indexOf('404') < 0) toast('Gagal muat laporan terbaru: ' + e.message, true);
  }
  renderDailyReport(latest);
  renderStatusServer(latest);
  renderTelemetri(latest);
  try {
    const hist = await apiJSON('/api/report/history?limit=10');
    renderRiwayat(hist);
  } catch (e) {
    renderRiwayat(null);
    toast('Gagal muat riwayat laporan: ' + e.message, true);
  }
}

async function refreshSemua() {
  const opts = arguments[0] || {};
  if (reportPolling) {
    if (!opts.silent) toast('Laporan sedang dibuat; tampilan akan dimuat ulang setelah selesai.');
    return;
  }
  if (refreshInFlight) return;
  refreshInFlight = true;
  const refreshButton = document.getElementById('btn-refresh-head');
  refreshButton.classList.add('is-loading');
  refreshButton.setAttribute('aria-busy', 'true');
  try {
    await Promise.all([muatHeaderStatus(), muatLaporan(), muatSesi(), muatPenerima(), muatJadwal()]);
    const now = new Date();
    const jam = [now.getHours(), now.getMinutes(), now.getSeconds()]
      .map(function (part) { return String(part).padStart(2, '0'); }).join(':');
    const refreshLabel = 'Tampilan dimuat ulang ' + jam;
    document.getElementById('last-refresh').textContent = refreshLabel;
    const sidebarRefresh = document.getElementById('sidebar-refresh');
    if (sidebarRefresh) sidebarRefresh.textContent = refreshLabel;
  } finally {
    refreshButton.classList.remove('is-loading');
    refreshButton.removeAttribute('aria-busy');
    refreshInFlight = false;
  }
}

function pauseAutoRefresh() {
  if (autoRefreshTimer) {
    clearInterval(autoRefreshTimer);
    autoRefreshTimer = null;
  }
}

function resumeAutoRefresh() {
  if (!autoRefreshTimer) {
    autoRefreshTimer = setInterval(function () { refreshSemua({ silent: true }); }, 30000);
  }
}

function stopReportPolling() {
  if (reportPollTimer) {
    clearInterval(reportPollTimer);
    reportPollTimer = null;
  }
  reportPolling = false;
  reportPollRequestInFlight = false;
  setRunButtonRunning(false);
  resumeAutoRefresh();
}

function startReportPolling(baseId) {
  if (reportPollTimer) clearInterval(reportPollTimer);
  reportPolling = true;
  reportPollBaseId = baseId == null ? null : String(baseId);
  reportPollRequestInFlight = false;
  pauseAutoRefresh();
  setRunButtonRunning(true);
  updateReportProgress({});
  reportPollTimer = setInterval(pollStatus, 3000);
}

async function buatLaporan() {
  if (reportPolling) return;
  let initialReportId = null;
  setRunButtonRunning(true);
  try {
    const initialStatus = await apiJSON('/api/report/status');
    initialReportId = initialStatus && initialStatus.report_id;
    const response = await apiFetch('/api/report/run', { method: 'POST' });
    if (response.status !== 202) throw new Error('Respons API tidak terduga: HTTP ' + response.status);
    startReportPolling(initialReportId);
  } catch (e) {
    if (e.status === 409) {
      toast('Siklus laporan sudah berjalan; status dipantau.');
      startReportPolling(initialReportId);
      return;
    }
    setRunButtonRunning(false);
    setReportProgressVisible(false);
    if (e.status === 401) {
      setReportState('GAGAL: API key salah', 'error');
      toast('API key salah', true);
    } else if (e.status === 503) {
      setReportState('GAGAL: siklus belum dikonfigurasi', 'error');
      toast('siklus laporan belum dikonfigurasi', true);
    } else {
      setReportState('GAGAL: ' + e.message, 'error');
      toast('Gagal membuat laporan: ' + e.message, true);
    }
  }
}

function reportIdAdvanced(currentId, initialId) {
  if (currentId == null) return false;
  if (initialId == null) {
    const currentNumber = Number(currentId);
    return Number.isFinite(currentNumber) && currentNumber > 0;
  }
  const currentNumber = Number(currentId);
  const initialNumber = Number(initialId);
  if (Number.isFinite(currentNumber) && Number.isFinite(initialNumber)) {
    return currentNumber > initialNumber;
  }
  return String(currentId) > String(initialId);
}

async function pollStatus() {
  if (!reportPolling || reportPollRequestInFlight) return;
  reportPollRequestInFlight = true;
  let status;
  try {
    status = await apiJSON('/api/report/status');
  } catch (e) {
    reportPollRequestInFlight = false;
    stopReportPolling();
    setReportProgressVisible(false);
    setReportState('GAGAL: status tidak terbaca', 'error');
    toast('Gagal baca status laporan: ' + e.message, true);
    return;
  }
  reportPollRequestInFlight = false;

  if (status && status.running) {
    updateReportProgress(status);
    return;
  }

  const currentId = status && status.report_id != null ? String(status.report_id) : null;
  const hasNewReport = reportIdAdvanced(currentId, reportPollBaseId);
  stopReportPolling();
  setReportProgressVisible(false);
  if (hasNewReport) {
    await muatLaporan();
    toast('Laporan baru siap');
  } else {
    setReportState('SIKLUS SELESAI TANPA LAPORAN BARU', 'held');
    toast('Siklus selesai tanpa laporan baru — telemetri portal tidak terbaca, laporan tidak dikirim', true);
  }
}
async function muatSesi() {
  try {
    const s = await apiJSON('/api/session/status');
    setBadge('badge-sesi', s && s.valid ? 'Sesi Portal: valid' : 'Sesi Portal: expired', !!(s && s.valid));
  } catch (e) {
    setBadge('badge-sesi', 'Sesi Portal: tidak terhubung', false);
  }
}

async function perbaruiSesi() {
  const token = document.getElementById('input-token').value.trim();
  if (!token) { toast('Token belum diisi', true); return; }
  try {
    await apiFetch('/api/session/token', {
      method: 'PUT',
      headers: headers({ 'Content-Type': 'application/json' }),
      body: JSON.stringify({ token: token })
    });
    toast('Token sesi portal disimpan');
    document.getElementById('input-token').value = '';
    muatSesi();
  } catch (e) {
    toast('Gagal perbarui sesi: ' + e.message, true);
  }
}

async function muatPenerima() {
  try {
    const list = await apiJSON('/api/recipients');
    const ul = document.getElementById('list-penerima');
    if (!list || !list.length) {
      ul.innerHTML = '<li>Belum ada penerima.</li>';
      return;
    }
    ul.innerHTML = list.map(function (r) {
      return '<li><span>' + (r.Name ? escapeHtml(r.Name) + ' ' : '') + escapeHtml(r.Phone) + '</span>' +
        ' <span>' +
        '<button class="btn btn-toggle" data-id="' + r.ID + '" data-aktif="' + (r.Active ? 1 : 0) + '">' +
        (r.Active ? 'Nonaktifkan' : 'Aktifkan') + '</button> ' +
        '<button class="btn btn-hapus-penerima" data-id="' + r.ID + '">Hapus</button>' +
        '</span></li>';
    }).join('');
    ul.querySelectorAll('.btn-toggle').forEach(function (b) {
      b.addEventListener('click', function () { togglePenerima(Number(b.dataset.id), b.dataset.aktif === '1'); });
    });
    ul.querySelectorAll('.btn-hapus-penerima').forEach(function (b) {
      b.addEventListener('click', function () {
        if (!confirm('Hapus penerima ini?')) return;
        apiFetch('/api/recipients/' + b.dataset.id, { method: 'DELETE' })
          .then(function () { toast('Penerima dihapus'); muatPenerima(); })
          .catch(function (e) { toast('Gagal hapus: ' + e.message, true); });
      });
    });
  } catch (e) {
    document.getElementById('list-penerima').innerHTML = '<li>Gagal muat penerima.</li>';
    toast('Gagal muat penerima: ' + e.message, true);
  }
}

async function togglePenerima(id, aktifSaatIni) {
  try {
    await apiFetch('/api/recipients/' + id, {
      method: 'PUT',
      headers: headers({ 'Content-Type': 'application/json' }),
      body: JSON.stringify({ active: !aktifSaatIni })
    });
    toast('Status penerima diubah');
    muatPenerima();
  } catch (e) {
    toast('Gagal ubah status: ' + e.message, true);
  }
}

async function tambahPenerima() {
  const nama = document.getElementById('input-nama').value.trim();
  const hp = document.getElementById('input-hp').value.trim();
  if (!hp) { toast('Nomor HP wajib diisi', true); return; }
  try {
    await apiFetch('/api/recipients', {
      method: 'POST',
      headers: headers({ 'Content-Type': 'application/json' }),
      body: JSON.stringify({ name: nama, phone: hp })
    });
    document.getElementById('input-nama').value = '';
    document.getElementById('input-hp').value = '';
    toast('Penerima ditambahkan');
    muatPenerima();
  } catch (e) {
    toast('Gagal tambah penerima: ' + e.message, true);
  }
}
async function muatJadwal() {
  try {
    const list = await apiJSON('/api/schedules');
    const ul = document.getElementById('list-jadwal');
    if (!list || !list.length) {
      ul.innerHTML = '<li>Belum ada jadwal.</li>';
      return;
    }
    ul.innerHTML = list.map(function (sc) {
      const id = recordId(sc);
      const name = scheduleName(sc);
      const cron = scheduleCron(sc);
      const active = scheduleActive(sc);
      return '<li data-id="' + escapeHtml(id) + '"><span>' + escapeHtml(name) + ' - <code>' + escapeHtml(cron) + '</code>' +
        (active ? '' : ' <em>(nonaktif)</em>') + '</span>' +
        '<span>' +
        '<button class="btn btn-edit-jadwal" type="button" data-action="edit" data-id="' + escapeHtml(id) + '">Edit Cron</button> ' +
        '<button class="btn btn-toggle-jadwal" type="button" data-action="toggle" data-id="' + escapeHtml(id) + '" data-aktif="' + (active ? 1 : 0) + '">' +
        (active ? 'Nonaktifkan' : 'Aktifkan') + '</button>' +
        '</span></li>';
    }).join('');
  } catch (e) {
    document.getElementById('list-jadwal').innerHTML = '<li>Gagal muat jadwal.</li>';
    toast('Gagal muat jadwal: ' + e.message, true);
  }
}

var _jadwalLast = [];

function parseCronTime(expression) {
  const fields = String(expression || '').trim().split(/\s+/);
  if (fields.length !== 5 || !/^\d+$/.test(fields[0]) || !/^\d+$/.test(fields[1]) ||
      fields[2] !== '*' || fields[3] !== '*' || fields[4] !== '*') return null;
  const minute = Number(fields[0]);
  const hour = Number(fields[1]);
  if (minute < 0 || minute > 59 || hour < 0 || hour > 23) return null;
  return { minute: minute, hour: hour };
}

function opsiWaktuCron(max, selected) {
  return Array.from({ length: max }, function (_, value) {
    const label = String(value).padStart(2, '0');
    return '<option value="' + value + '"' + (value === selected ? ' selected' : '') + '>' + label + '</option>';
  }).join('');
}

async function editJadwal(id) {
  const sc = _jadwalLast && _jadwalLast.filter(function (x) { return String(recordId(x)) === String(id); })[0];
  if (!sc) {
    toast('Jadwal tidak ditemukan. Muat ulang tampilan lalu coba lagi.', true);
    return;
  }
  const row = Array.from(document.querySelectorAll('#list-jadwal li'))
    .filter(function (item) { return item.dataset.id === String(id); })[0];
  if (!row) return;
  row.classList.add('is-editing');
  const name = scheduleName(sc) || 'jadwal';
  const time = parseCronTime(scheduleCron(sc));
  const editorControl = time ?
    '<div class="cron-time-controls">' +
    '<label><span>Jam</span><select class="cron-hour-select" aria-label="Jam">' + opsiWaktuCron(24, time.hour) + '</select></label>' +
    '<label><span>Menit</span><select class="cron-minute-select" aria-label="Menit">' + opsiWaktuCron(60, time.minute) + '</select></label>' +
    '</div>' :
    '<label class="cron-expression-control" for="cron-edit-' + escapeHtml(id) + '"><span>Ekspresi Cron</span>' +
    '<input id="cron-edit-' + escapeHtml(id) + '" class="cron-editor-input" value="' + escapeHtml(scheduleCron(sc)) + '"></label>';
  row.innerHTML = '<span class="cron-editor-label">Edit Cron — ' + escapeHtml(name) + '</span>' +
    '<div class="cron-editor">' +
    editorControl +
    '<button class="btn btn-accent" type="button" data-action="save" data-id="' + escapeHtml(id) + '">Simpan</button>' +
    '<button class="btn btn-secondary" type="button" data-action="cancel" data-id="' + escapeHtml(id) + '">Batal</button>' +
    '</div>';
  const firstControl = row.querySelector('select, input');
  firstControl.focus();
  if (typeof firstControl.select === 'function') firstControl.select();
}

async function simpanJadwal(id) {
  const sc = _jadwalLast && _jadwalLast.filter(function (x) { return String(recordId(x)) === String(id); })[0];
  const row = Array.from(document.querySelectorAll('#list-jadwal li'))
    .filter(function (item) { return item.dataset.id === String(id); })[0];
  if (!sc || !row) return;
  const hourSelect = row.querySelector('.cron-hour-select');
  const minuteSelect = row.querySelector('.cron-minute-select');
  const input = row.querySelector('.cron-editor-input');
  const cron = hourSelect && minuteSelect ?
    minuteSelect.value + ' ' + hourSelect.value + ' * * *' : input && input.value.trim();
  if (!cron) {
    toast('Cron expression wajib diisi', true);
    if (input) input.focus();
    return;
  }
  try {
    await apiFetch('/api/schedules', {
      method: 'PUT',
      headers: headers({ 'Content-Type': 'application/json' }),
      body: JSON.stringify({ name: scheduleName(sc), cron_expr: cron, active: scheduleActive(sc) })
    });
    toast('Jadwal diperbarui');
    await muatJadwal();
  } catch (e) {
    toast('Gagal edit jadwal: ' + e.message, true);
  }
}

function batalEditJadwal() {
  muatJadwal();
}

async function toggleJadwal(id, aktifSaatIni) {
  const sc = _jadwalLast && _jadwalLast.filter(function (x) { return String(recordId(x)) === String(id); })[0];
  if (!sc) {
    toast('Jadwal tidak ditemukan. Muat ulang tampilan lalu coba lagi.', true);
    return;
  }
  try {
    await apiFetch('/api/schedules', {
      method: 'PUT',
      headers: headers({ 'Content-Type': 'application/json' }),
      body: JSON.stringify({ name: scheduleName(sc), cron_expr: scheduleCron(sc), active: !aktifSaatIni })
    });
    toast('Status jadwal diubah');
    muatJadwal();
  } catch (e) {
    toast('Gagal ubah jadwal: ' + e.message, true);
  }
}

// simpan cache list jadwal untuk edit/toggle
const _apiJSONAsli = apiJSON;
apiJSON = async function (path, opts) {
  const out = await _apiJSONAsli(path, opts);
  if (path === '/api/schedules' && Array.isArray(out)) _jadwalLast = out;
  return out;
};
async function tampilkanQR() {
  const area = document.getElementById('qr-area');
  try {
    const resp = await apiFetch('/api/qr');
    if ((resp.headers.get('content-type') || '').indexOf('image/') === 0) {
      const blob = await resp.blob();
      const url = URL.createObjectURL(blob);
      area.innerHTML = '<img src="' + url + '" alt="QR WhatsApp">';
    } else {
      const j = await resp.json();
      area.innerHTML = '<p>' + escapeHtml(j.message || 'QR belum tersedia') + '</p>';
    }
  } catch (e) {
    area.innerHTML = '<p>' + escapeHtml(e.message) + '</p>';
    toast('Gagal tampilkan QR: ' + e.message, true);
  }
}

async function logoutWA() {
  if (!confirm('Putus sesi WhatsApp? Perlu scan QR baru setelah logout.')) return;
  try {
    await apiFetch('/api/wa/logout', { method: 'POST' });
    toast('Logout WhatsApp dikirim');
    areaQRKosong();
    muatHeaderStatus();
  } catch (e) {
    toast('Gagal logout WA: ' + e.message, true);
  }
}

function areaQRKosong() {
  document.getElementById('qr-area').innerHTML = '';
}

function simpanApiKey() {
  const v = document.getElementById('input-api-key').value.trim();
  setApiKey(v);
  toast('API key disimpan');
  refreshSemua();
}

function tutupModal() {
  document.getElementById('modal').classList.add('hidden');
}

function init() {
  // API key dari localStorage (diisi operator lewat kolom di dashboard).
  const inp = document.getElementById('input-api-key');
  inp.value = getApiKey();

  document.getElementById('btn-simpan-apikey').addEventListener('click', simpanApiKey);
  document.getElementById('btn-refresh-head').addEventListener('click', function () { refreshSemua(); });
  document.querySelector('.side-nav-links').addEventListener('click', function (event) {
    const link = event.target.closest('a[href^="#"]');
    if (!link) return;
    this.querySelectorAll('a').forEach(function (item) {
      item.classList.toggle('is-current', item === link);
      if (item === link) item.setAttribute('aria-current', 'page');
      else item.removeAttribute('aria-current');
    });
  });
  document.getElementById('btn-refresh-report').addEventListener('click', buatLaporan);
  document.getElementById('btn-reload-report').addEventListener('click', muatLaporan);
  document.getElementById('btn-copy-report').addEventListener('click', copyDailyReport);
  document.getElementById('btn-perbarui-sesi').addEventListener('click', perbaruiSesi);
  document.getElementById('btn-tambah-penerima').addEventListener('click', tambahPenerima);
  document.getElementById('list-jadwal').addEventListener('click', function (event) {
    const button = event.target.closest('button[data-action]');
    if (!button) return;
    if (button.dataset.action === 'edit') editJadwal(button.dataset.id);
    if (button.dataset.action === 'save') simpanJadwal(button.dataset.id);
    if (button.dataset.action === 'cancel') batalEditJadwal();
    if (button.dataset.action === 'toggle') {
      toggleJadwal(button.dataset.id, button.dataset.aktif === '1');
    }
  });
  document.getElementById('btn-qr').addEventListener('click', tampilkanQR);
  document.getElementById('btn-logout-wa').addEventListener('click', logoutWA);
  document.getElementById('btn-tutup-modal').addEventListener('click', tutupModal);
  document.getElementById('btn-tutup-modal-footer').addEventListener('click', tutupModal);
  document.getElementById('modal').addEventListener('click', function (ev) {
    if (ev.target === this) tutupModal();
  });

  refreshSemua();
  resumeAutoRefresh();
}

document.addEventListener('DOMContentLoaded', init);
