// WhatsApp gateway service untuk stack monitoring (diadaptasi dari
// smsgateway-grita/apps/whatsapp-service, Node.js + Baileys).
//
// Perubahan dari referensi:
// - Penyimpanan sesi Baileys memakai file di volume (bukan PostgreSQL)
//   agar stack monitoring tidak butuh database tambahan.
// - Endpoint dikurangi: health (/ping), status sesi (/api/status),
//   QR (/api/qr), kirim text (/send-text), logout (/api/logout).
// - Format /send-text {phone, message} dipakai monitoring-api.
const fs = require('fs');
const path = require('path');
const express = require('express');
const QRCode = require('qrcode');
const pino = require('pino');
const {
  default: makeWASocket,
  DisconnectReason,
  fetchLatestBaileysVersion,
  makeCacheableSignalKeyStore,
  useMultiFileAuthState,
} = require('@whiskeysockets/baileys');

const PORT = process.env.WA_SERVICE_PORT || 3101;
const API_KEY = process.env.WA_API_KEY;
const AUTH_DIR = process.env.WA_AUTH_DIR || '/data/baileys-session';
// Batas waktu keras pengiriman satu pesan (ms) supaya /send-text selalu
// menjawab walau Baileys menggantung.
const SEND_TIMEOUT_MS = 45000;

// Cache pesan yang baru dikirim. Baileys memerlukannya untuk menjawab retry
// receipt dari penerima: tanpa getMessage, penerima tidak bisa meminta ulang
// isi pesan dan melihat "Waiting for this message. This may take a while."
const SENT_CACHE_MAX = 500;
const sentMessages = new Map();

function rememberSent(id, message) {
  if (!id || !message) return;
  sentMessages.set(id, message);
  if (sentMessages.size > SENT_CACHE_MAX) {
    const oldest = sentMessages.keys().next().value;
    sentMessages.delete(oldest);
  }
}

// Status ack Baileys: 0 error, 1 pending, 2 server, 3 diantar, 4 dibaca.
function ackLabel(status) {
  return { 0: 'error', 1: 'pending', 2: 'server', 3: 'diantar', 4: 'dibaca' }[status] || String(status);
}

if (!API_KEY) {
  console.error('Environment variable WA_API_KEY wajib diatur.');
  process.exit(1);
}

const logger = pino({ level: process.env.LOG_LEVEL || 'info' });

// ===== Baileys client =====
class BaileysClient {
  constructor() {
    this.sock = null;
    this.qrCode = null;
    this.status = 'disconnected';
    this.retryCount = 0;
    this.maxRetries = 10;
    // Guard single-flight: cegah dua proses connect/socket berjalan bersamaan.
    this.connecting = false;
    this.reconnectTimer = null;
    // Satu kejadian 'close' hanya boleh memicu satu jalur reconnect.
    this.closeHandled = false;
    // Generasi socket dipakai untuk mengabaikan event dari socket lama.
    this.socketGeneration = 0;
  }

  async connect() {
    // Single-flight: bila inisialisasi masih berjalan, permintaan connect
    // berikutnya diabaikan supaya tidak ada dua socket bersamaan.
    if (this.connecting) {
      logger.info('connect() diabaikan: inisialisasi sebelumnya masih berjalan');
      return;
    }
    this.connecting = true;
    this.closeHandled = false;
    // Batalkan reconnect lama dan bersihkan socket lama sebelum membuat baru.
    this._cancelReconnect();
    this._teardownSocket();
    const generation = this.socketGeneration;
    try {
      this.status = 'connecting';
      const { state, saveCreds } = await useMultiFileAuthState(AUTH_DIR);
      const { version } = await fetchLatestBaileysVersion();

      const sock = makeWASocket({
        version,
        logger: pino({ level: 'silent' }),
        auth: {
          creds: state.creds,
          keys: makeCacheableSignalKeyStore(state.keys, pino({ level: 'silent' })),
        },
        printQRInTerminal: false,
        generateHighQualityLinkPreview: false,
        browser: ['GRITA-Monitoring', 'Chrome', '22.0'],
        connectTimeoutMs: 60000,
        defaultQueryTimeoutMs: 60000,
        retryRequestDelayMs: 500,
        // Wajib untuk menangani retry receipt dari penerima (penyebab pesan
        // tampil "Waiting for this message" bila tidak ada).
        getMessage: async (key) => {
          if (!key || !key.id) return undefined;
          return sentMessages.get(key.id);
        },
      });

      this.sock = sock;
      sock.ev.on('creds.update', saveCreds);
      sock.ev.on('connection.update', (u) => {
        // Abaikan event sisa dari socket yang sudah digantikan.
        if (generation !== this.socketGeneration) return;
        this._handleConnectionUpdate(u);
      });
      // Jejak status ack: tanpa ini, pesan yang tidak bisa didekripsi penerima
      // terlihat "sukses" di log padahal belum sampai.
      sock.ev.on('messages.update', (updates) => {
        if (generation !== this.socketGeneration) return;
        for (const u of updates) {
          const status = u && u.update ? u.update.status : undefined;
          if (status === undefined) continue;
          logger.info(`status pesan ${u.key ? u.key.id : '?'}: ${ackLabel(status)}`);
        }
      });
      logger.info('Baileys client diinisialisasi');
    } catch (err) {
      logger.error(`Gagal inisialisasi Baileys: ${err.message}`);
      this.status = 'disconnected';
      this._scheduleReconnect();
    } finally {
      this.connecting = false;
    }
  }

  _handleConnectionUpdate(update) {
    const { connection, lastDisconnect, qr } = update;

    if (qr) {
      this.qrCode = qr;
      this.status = 'qr';
      logger.info('QR siap, scan via GET /api/qr');
    }

    if (connection === 'close') {
      const statusCode = lastDisconnect?.error?.output?.statusCode;
      logger.warn(`WhatsApp terputus [${statusCode}]`);
      this.status = 'disconnected';
      this.qrCode = null;
      // Hindari beberapa jalur reconnect dari satu kejadian close.
      if (this.closeHandled || this.connecting || this.reconnectTimer) {
        return;
      }
      this.closeHandled = true;
      if (statusCode === DisconnectReason.loggedOut) {
        // Sesi tidak valid: hapus creds basi, lalu reconnect berjeda
        // supaya QR baru bisa muncul (tanpa loop 401 panas).
        this._recoverLoggedOut();
      } else {
        this._scheduleReconnect();
      }
      return;
    }

    if (connection === 'open') {
      this.status = 'connected';
      this.qrCode = null;
      this.retryCount = 0;
      logger.info(`WhatsApp terhubung: ${this.sock?.user?.id || 'unknown'}`);
    }
  }

  // Alur loggedOut: bersihkan sesi sekali, reset hitungan retry, lalu
  // reconnect setelah jeda singkat agar QR baru diterbitkan.
  async _recoverLoggedOut() {
    this.retryCount = 0;
    await this.clearSession();
    this._scheduleReconnect(3000);
  }

  // Backoff eksponensial: 2s, 4s, 8s ... maks 60s.
  // fixedDelayMs dipakai untuk jeda tetap (mis. sesudah loggedOut).
  _scheduleReconnect(fixedDelayMs) {
    this._cancelReconnect();
    if (this.retryCount >= this.maxRetries) {
      this.retryCount = 0;
    }
    this.retryCount++;
    const delay = fixedDelayMs || Math.min(1000 * Math.pow(2, this.retryCount), 60000);
    logger.info(`Reconnect dalam ${delay / 1000}s (${this.retryCount}/${this.maxRetries})`);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delay);
  }

  _cancelReconnect() {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  // Lepas socket lama: naikkan generasi agar event sisa diabaikan, lepas
  // listener, lalu tutup socket agar tidak ada dua koneksi aktif.
  _teardownSocket() {
    this.socketGeneration++;
    const old = this.sock;
    this.sock = null;
    if (!old) return;
    try {
      old.ev.removeAllListeners('connection.update');
      old.ev.removeAllListeners('creds.update');
    } catch (err) {
      // Event emitter sudah tidak tersedia, abaikan.
    }
    try {
      old.end(undefined);
    } catch (err) {
      // Socket mungkin sudah tertutup, abaikan.
    }
  }

  // Hapus ISI direktori sesi, bukan direktorinya: AUTH_DIR adalah mount
  // point volume sehingga rmdir pada direktori itu sendiri selalu gagal
  // dengan EBUSY.
  async clearSession() {
    try {
      if (!fs.existsSync(AUTH_DIR)) {
        logger.info('Direktori sesi belum ada, tidak ada yang dihapus');
        return;
      }
      const entries = fs.readdirSync(AUTH_DIR);
      for (const name of entries) {
        fs.rmSync(path.join(AUTH_DIR, name), { recursive: true, force: true });
      }
      this.qrCode = null;
      logger.info(`Sesi Baileys dihapus (${entries.length} entri)`);
    } catch (err) {
      logger.error(`Gagal hapus sesi: ${err.message}`);
    }
  }

  // listGroups mengembalikan grup yang diikuti akun bot. JID grup tidak
  // tampil di UI WhatsApp, jadi ini cara andal menentukannya.
  async listGroups() {
    if (this.status !== 'connected' || !this.sock) {
      throw new Error('belum terhubung (status: ' + this.status + ')');
    }
    const all = await this.sock.groupFetchAllParticipating();
    return Object.values(all || {}).map((g) => ({
      jid: g.id,
      name: g.subject || '',
      participants: Array.isArray(g.participants) ? g.participants.length : 0,
      hanyaAdmin: Boolean(g.announce),
    }));
  }

  // joinGroupByInvite menerima tautan undangan (https://chat.whatsapp.com/XXX)
  // atau kodenya saja, lalu mengembalikan JID grup.
  async joinGroupByInvite(link) {
    if (this.status !== 'connected' || !this.sock) {
      throw new Error('belum terhubung (status: ' + this.status + ')');
    }
    const code = String(link || '').trim().split('/').pop().split('?')[0];
    if (!code) throw new Error('kode undangan kosong');
    return this.sock.groupAcceptInvite(code);
  }

  async sendMessage(chatId, text) {
    if (this.status !== 'connected' || !this.sock) {
      return { success: false, error: 'Not connected. Status: ' + this.status };
    }
    try {
      // Pengaman keras: sock.sendMessage() bisa menggantung tanpa batas bila
      // server WhatsApp tidak pernah membalas, sehingga POST /send-text juga
      // tidak pernah menjawab. Batasi SEND_TIMEOUT_MS lalu laporkan gagal
      // supaya endpoint selalu mengembalikan respons (503) tepat waktu.
      let timeoutHandle;
      const timeoutPromise = new Promise((resolve) => {
        timeoutHandle = setTimeout(
          () => resolve({ success: false, error: 'timeout kirim pesan' }),
          SEND_TIMEOUT_MS
        );
      });
      const sendPromise = this.sock.sendMessage(chatId, { text }).then(
        (result) => ({ success: true, result }),
        (err) => ({ success: false, error: err.message })
      );
      try {
        const outcome = await Promise.race([sendPromise, timeoutPromise]);
        if (!outcome.success) {
          logger.error(`Gagal kirim ke ${chatId}: ${outcome.error}`);
          return { success: false, error: outcome.error };
        }
        logger.info(`Pesan terkirim ke ${chatId} (id: ${outcome.result.key.id})`);
        // Simpan isi pesan supaya permintaan ulang dari penerima bisa dijawab.
        rememberSent(outcome.result.key.id, outcome.result.message);
        return { success: true, messageId: outcome.result.key.id };
      } finally {
        clearTimeout(timeoutHandle);
      }
    } catch (err) {
      logger.error(`Gagal kirim ke ${chatId}: ${err.message}`);
      return { success: false, error: err.message };
    }
  }

  getStatus() {
    return {
      status: this.status,
      user: this.status === 'connected' ? this.sock?.user || null : null,
      retryCount: this.retryCount,
      hasQR: !!this.qrCode,
    };
  }

  async disconnect() {
    this._cancelReconnect();
    this._teardownSocket();
    this.status = 'disconnected';
    this.qrCode = null;
  }

  // Logout manual: putuskan socket, hapus sesi, lalu reconnect setelah
  // jeda agar QR baru diterbitkan.
  async logout() {
    this.retryCount = 0;
    await this.disconnect();
    await this.clearSession();
    this.closeHandled = false;
    this._scheduleReconnect(2000);
  }
}

// ===== Validasi (dari referensi validate.js) =====
const JID_RE = /^\d{10,15}@s\.whatsapp\.net$/;
const DIGITS_RE = /^\d{10,15}$/;
const MAX_TEXT_LEN = 4096;

function normalizeChatId(chatId) {
  if (typeof chatId !== 'string') return null;
  const trimmed = chatId.trim();
  if (JID_RE.test(trimmed)) return trimmed;
  if (/^\d{10,15}@c\.us$/.test(trimmed)) return trimmed.replace(/@c\.us$/, '@s.whatsapp.net');
  // JID grup: <id>@g.us. Bentuk lama bisa memuat tanda hubung. Dikirim apa
  // adanya karena nomor grup bukan nomor telepon (tidak ada konversi 0 -> 62).
  if (/^[0-9-]{10,40}@g\.us$/.test(trimmed)) return trimmed;
  const digits = trimmed.replace(/^\+/, '');
  if (DIGITS_RE.test(digits)) {
    // Nomor Indonesia sering disimpan dalam format lokal (08xx). JID WhatsApp
    // wajib format internasional tanpa awalan nol: 628xx. Tanpa konversi ini
    // sock.sendMessage menggantung (JID tidak dikenal) sampai batas waktu.
    const intl = digits.startsWith('0') ? `62${digits.slice(1)}` : digits;
    return `${intl}@s.whatsapp.net`;
  }
  return null;
}

function isValidText(text) {
  return typeof text === 'string' && text.length > 0 && text.length <= MAX_TEXT_LEN;
}

// ===== HTTP app =====
const waClient = new BaileysClient();
const app = express();
app.use(express.json());

function requireApiKey(req, res, next) {
  if (req.headers['x-api-key'] !== API_KEY) {
    return res.status(401).json({ success: false, error: 'API key tidak valid' });
  }
  next();
}

// Health check publik (dipakai docker-compose healthcheck).
app.get('/ping', (_req, res) => res.send('pong'));

// Endpoint laporan (/send-text dan /api/status) dan mutasi lain wajib API key.
app.get('/api/status', requireApiKey, (_req, res) => res.json(waClient.getStatus()));

app.get('/api/qr', requireApiKey, async (_req, res) => {
  const qrString = waClient.qrCode;
  if (!qrString) {
    if (waClient.status === 'connected') {
      return res.json({ success: false, message: 'Sudah terhubung, tidak perlu scan QR.' });
    }
    return res.status(404).json({ success: false, message: 'QR belum tersedia, tunggu beberapa detik.' });
  }
  try {
    const qrImage = await QRCode.toBuffer(qrString, { type: 'png', width: 300, margin: 2 });
    res.set('Content-Type', 'image/png').send(qrImage);
  } catch (err) {
    logger.error(`Gagal generate QR: ${err.message}`);
    res.status(500).json({ success: false, error: 'Gagal generate QR code' });
  }
});

// Endpoint utama yang dipakai monitoring-api: {phone, message}.
app.post('/send-text', requireApiKey, async (req, res) => {
  const { phone, message } = req.body || {};
  const chatId = normalizeChatId(phone);
  if (!chatId || !isValidText(message)) {
    return res.status(400).json({ success: false, error: 'phone atau message tidak valid.' });
  }
  const result = await waClient.sendMessage(chatId, message);
  if (result.success) {
    return res.json({ success: true, messageId: result.messageId, chatId });
  }
  return res.status(503).json({ success: false, error: result.error });
});

app.post('/api/logout', requireApiKey, async (_req, res) => {
  try {
    await waClient.logout();
    res.json({ success: true, message: 'Logged out. Scan QR baru untuk reconnect.' });
  } catch (err) {
    res.status(500).json({ success: false, error: err.message });
  }
});

// Grup yang diikuti akun bot: {success, count, groups:[{jid,name,participants}]}.
app.get('/api/groups', requireApiKey, async (_req, res) => {
  try {
    const groups = await waClient.listGroups();
    res.json({ success: true, count: groups.length, groups });
  } catch (err) {
    res.status(503).json({ success: false, error: err.message });
  }
});

// Masuk ke grup lewat tautan/kode undangan: {link}.
app.post('/api/groups/join', requireApiKey, async (req, res) => {
  try {
    const jid = await waClient.joinGroupByInvite((req.body || {}).link);
    res.json({ success: true, jid });
  } catch (err) {
    res.status(400).json({ success: false, error: err.message });
  }
});

async function start() {
  await waClient.connect();
  app.listen(PORT, '0.0.0.0', () => {
    logger.info(`whatsapp-service berjalan di http://0.0.0.0:${PORT}`);
  });
}

const shutdown = async () => {
  logger.info('Shutdown...');
  await waClient.disconnect();
  process.exit(0);
};
process.on('SIGTERM', shutdown);
process.on('SIGINT', shutdown);

start();
