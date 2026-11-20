function encodeCloudMetaHeader(obj) {
  const bytes = new TextEncoder().encode(JSON.stringify(obj));
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}

function cloudMimeForName(name, fallback = 'application/octet-stream') {
  const lower = String(name || '').toLowerCase();
  if (lower.endsWith('.docx')) return 'application/vnd.openxmlformats-officedocument.wordprocessingml.document';
  if (lower.endsWith('.pdf')) return 'application/pdf';
  if (lower.endsWith('.xlsx')) return 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet';
  if (lower.endsWith('.xls')) return 'application/vnd.ms-excel';
  if (lower.endsWith('.csv')) return 'text/csv';
  if (lower.endsWith('.pptx')) return 'application/vnd.openxmlformats-officedocument.presentationml.presentation';
  if (lower.endsWith('.txt')) return 'text/plain';
  if (lower.endsWith('.json')) return 'application/json';
  if (lower.endsWith('.html')) return 'text/html';
  if (lower.endsWith('.png')) return 'image/png';
  if (lower.endsWith('.jpg') || lower.endsWith('.jpeg')) return 'image/jpeg';
  if (lower.endsWith('.webp')) return 'image/webp';
  return fallback;
}

function cleanCloudSegment(raw, fallback = '未命名') {
  const cleaned = String(raw ?? '')
    .replace(/[\x00-\x1f\x7f-\x9f\\/:*?"<>|]+/g, '_')
    .trim();
  const sliced = Array.from(cleaned).slice(0, 120).join('').trim();
  if (!sliced || sliced === '.' || sliced === '..') return fallback;
  return sliced;
}

function safeCloudFilename(name, fallback = 'file') {
  const raw = String(name ?? '').trim();
  const dot = raw.lastIndexOf('.');
  if (dot > 0 && dot < raw.length - 1) {
    const ext = raw.slice(dot).replace(/[\x00-\x1f\x7f-\x9f\\/:*?"<>|\s]+/g, '');
    const maxBase = Math.max(1, 120 - ext.length);
    const baseRaw = cleanCloudSegment(raw.slice(0, dot), fallback);
    const base = Array.from(baseRaw).slice(0, maxBase).join('').trim() || fallback;
    return ext ? base + ext : base;
  }
  return cleanCloudSegment(raw, fallback);
}

function safeDocxFilename(baseName, suffix = '') {
  const sfx = String(suffix || '');
  const maxBase = Math.max(1, 120 - sfx.length - 5);
  const baseRaw = cleanCloudSegment(baseName, '未命名试卷');
  const base = Array.from(baseRaw).slice(0, maxBase).join('').trim() || '未命名试卷';
  return base + sfx + '.docx';
}

function cleanCloudRelDirClient(relDir) {
  return String(relDir ?? '')
    .replace(/\\/g, '/')
    .split('/')
    .map(seg => seg.trim())
    .filter(seg => seg && seg !== '.' && seg !== '..')
    .map(seg => cleanCloudSegment(seg, ''))
    .filter(Boolean)
    .join('/');
}

function cloudFormatSize(bytes) {
  const n = Number(bytes) || 0;
  if (n <= 0) return '';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / (1024 * 1024)).toFixed(1) + ' MB';
}

function isCloudLanIPHost(host) {
  if (/^10\./.test(host) || /^192\.168\./.test(host)) return true;
  const m = /^172\.(\d+)\./.exec(host);
  if (m) {
    const second = Number(m[1]);
    return second >= 16 && second <= 31;
  }
  return false;
}

const cloudDrive = {
  data() {
    const defaultGoogleMode = isCloudLanIPHost(location.hostname) ? 'manual' : 'web';
    return {
      cloudLoaded: false,
      cloudConnections: [],
      cloudShared: {
        googleReady: false,
        baiduReady: false,
        baiduAppDir: '/apps/Biofolio',
        allowPrivateWebdav: false,
        proxyConfigured: false,
        proxyScope: 'google'
      },
      cloudNotice: '',
      cloudBusyId: '',
      cloudShowAdd: false,
      cloudAddTab: 'webdav',
      cloudEditMetaId: '',
      cloudEditMetaForm: { name: '', rootDir: '', isDefault: false, useProxy: false },

      cloudWebdavForm: { id: '', name: '', baseUrl: '', username: '', password: '', rootDir: '', isDefault: false, useProxy: false },
      cloudWebdavBusy: false,
      cloudWebdavNotice: '',

      cloudGoogleMode: defaultGoogleMode,
      cloudGoogleName: '我的 Google Drive',
      cloudGoogleRootDir: 'Biofolio',
      cloudGoogleShowCustom: false,
      cloudGoogleCustomId: '',
      cloudGoogleCustomSecret: '',
      cloudGoogleState: '',
      cloudGoogleAuthUrl: '',
      cloudGoogleRedirectUri: '',
      cloudGoogleManualInput: '',
      cloudGoogleBusy: false,
      cloudGoogleNotice: '',

      cloudBaiduName: '我的百度网盘',
      cloudBaiduRootDir: '',
      cloudBaiduUseProxy: false,
      cloudBaiduShowCustom: false,
      cloudBaiduCustomKey: '',
      cloudBaiduCustomSecret: '',
      cloudBaiduCustomDir: '',
      cloudBaiduSessionId: '',
      cloudBaiduQR: '',
      cloudBaiduUserCode: '',
      cloudBaiduVerifyUrl: '',
      cloudBaiduAppDir: '',
      cloudBaiduBusy: false,
      cloudBaiduNotice: '',

      cloudAdminForm: {
        proxyUrl: '',
        proxyScope: 'google',
        allowPrivateWebdav: false,
        prefixesText: '',
        googleClientId: '',
        googleClientSecret: '',
        googleHasSecret: false,
        googleRedirectUri: '',
        baiduAppKey: '',
        baiduSecretKey: '',
        baiduHasSecret: false,
        baiduAppDir: '/apps/Biofolio',
        envProxy: '',
        envAllowPrivateWebdav: false,
        envGoogleReady: false,
        envBaiduReady: false
      },
      cloudAdminBusy: false,
      cloudAdminNotice: '',

      cloudAutoBackupForm: { enabled: false, connectionId: '', dir: '', hour: 3 },
      cloudAutoBackupState: { lastAttemptDate: '', lastRunAt: '', lastSuccessAt: '', lastStatus: '', lastMessage: '' },
      cloudAutoBackupBusy: false,
      cloudAutoBackupNotice: '',

      cloudSyncStatus: {
        ok: true,
        configured: false,
        connectionId: '',
        rootDir: 'Biofolio',
        syncing: false,
        dirty: false,
        pendingAt: '',
        lastSyncAt: '',
        lastSyncError: '',
        local: { stateVersion: 0, recordCount: 0, syncVersion: 0 },
        remote: {
          checked: false,
          checkedAt: '',
          exists: false,
          empty: false,
          syncVersion: 0,
          updatedAt: '',
          recordCount: 0,
          shardCount: 0,
          error: ''
        },
        status: 'unconfigured'
      },
      cloudSyncLoaded: false,
      cloudSyncBusy: false,
      cloudSyncNotice: '',
      cloudSyncEmptyRemoteAlert: false,
      cloudSyncRestoreMode: 'merge',
      cloudSyncRestoreError: '',

      cloudDialogMode: 'pick',
      cloudDialogTitle: '从我的云盘选择',
      cloudDialogConnId: '',
      cloudDialogDir: '',
      cloudDialogDirName: '根目录',
      cloudDialogParentDir: '',
      cloudDialogCrumbs: [],
      cloudDialogEntries: [],
      cloudDialogLoading: false,
      cloudDialogError: '',
      cloudDialogAccept: [],
      cloudDialogMultiple: false,
      cloudDialogSelected: [],
      cloudDialogFilename: '',
      cloudDialogOverwrite: false,
      cloudDialogShowNewFolder: false,
      cloudDialogNewFolder: '',
      cloudDialogTransferBusy: false,
      cloudDialogBackupKind: 'json'
    };
  },
  computed: {
    cloudAvailable() {
      return Boolean(this.libraryServer);
    },
    cloudDefaultConnection() {
      return this.cloudConnections.find(c => c.isDefault) || this.cloudConnections[0] || null;
    },
    cloudCurrentConnection() {
      return this.cloudConnections.find(c => c.id === this.cloudDialogConnId) || this.cloudDefaultConnection;
    },
    cloudCallbackHint() {
      const prefix = location.pathname.startsWith('/biology/') ? '/biology' : '';
      return location.origin + prefix + '/library/cloud/google/callback';
    },
    cloudSyncStatusLabel() {
      const st = this.cloudSyncStatus || {};
      if (!st.configured || st.status === 'unconfigured') return '未连接';
      if (st.syncing) return '同步中';
      if (st.status === 'error') return '同步异常';
      if (st.status === 'remote_empty_local_populated') return '远端同步库为空（已保护本地数据）';
      if (st.status === 'remote_ahead') return '远端有更新';
      if (st.status === 'diverged') return '本地与远端均有改动';
      if (st.status === 'local_ahead' || st.dirty) return '待同步';
      if (st.status === 'in_sync') return '已同步';
      return '已就绪';
    },
    cloudSyncBadgeClass() {
      const st = this.cloudSyncStatus || {};
      if (st.syncing) return 'warn syncing';
      if (st.status === 'error') return 'danger';
      if (['local_ahead', 'remote_ahead', 'diverged', 'remote_empty_local_populated'].includes(st.status) || st.dirty) {
        return 'warn';
      }
      if (st.status === 'in_sync') return 'ok';
      return '';
    },
    cloudSyncSummaryText() {
      const st = this.cloudSyncStatus || {};
      const dir = (st.rootDir || 'Biofolio') + '/同步库/';
      const localCnt = st.local?.recordCount ?? 0;
      const remoteCnt = st.remote?.error
        ? '检查失败'
        : (st.remote?.exists ? `${st.remote.recordCount} 条` : (st.remote?.checked ? '空' : '未检查'));
      const last = st.lastSyncAt ? this.formatCloudSyncTime(st.lastSyncAt) : '尚未同步';
      return `Google Drive 同步（${dir}） · 本地 ${localCnt} 条 / 远端 ${remoteCnt} · 上次同步：${last}`;
    }
  },
  mounted() {
    this._cloudResolve = null;
    this._cloudPendingBlob = null;
    this._cloudBaiduTimer = null;
    this._cloudSyncDebounceTimer = null;
    this._cloudSyncPollTimer = null;
    this._cloudSyncPollCount = 0;
    this._cloudSyncUnmounted = false;
    this._cloudSyncStatusInFlight = null;
    this._onCloudSyncMutation = () => this.onLibraryMutationSync();
    try { syncActivityHandler = this._onCloudSyncMutation; } catch {}
    this._cloudOnOAuthMsg = data => {
      if (!data || typeof data !== 'object') return;
      if (data.ok) {
        this.cloudGoogleState = '';
        this.cloudGoogleAuthUrl = '';
        this.cloudGoogleManualInput = '';
        this.cloudGoogleRootDir = 'Biofolio';
        this.cloudGoogleNotice = data.name ? '已绑定 ' + data.name : 'Google Drive 已成功连接。';
        this.cloudShowAdd = false;
        void this.loadCloudConfig();
        void this.loadCloudSyncStatus();
        this.notify(this.cloudGoogleNotice);
      } else if (data.error) {
        this.cloudGoogleNotice = data.error;
      }
    };
    if (typeof BroadcastChannel !== 'undefined') {
      this._cloudChannel = new BroadcastChannel('biofolio-cloud-oauth');
      this._cloudChannel.addEventListener('message', e => this._cloudOnOAuthMsg(e.data));
    }
    this._cloudWinMsg = e => {
      if (e.origin === location.origin && e.data?.type === 'biofolio-cloud-oauth') {
        this._cloudOnOAuthMsg(e.data);
      }
    };
    window.addEventListener('message', this._cloudWinMsg);
    if (this.cloudAvailable) {
      void this.loadCloudConfig();
      void this.loadCloudSyncStatus();
    }
  },
  beforeUnmount() {
    this._cloudSyncUnmounted = true;
    this.stopCloudSyncTimers();
    try {
      if (typeof syncActivityHandler !== 'undefined' && syncActivityHandler === this._onCloudSyncMutation) {
        syncActivityHandler = null;
      }
    } catch {}
    this.stopBaiduPolling();
    if (this._cloudChannel) {
      try { this._cloudChannel.close(); } catch {}
      this._cloudChannel = null;
    }
    if (this._cloudWinMsg) window.removeEventListener('message', this._cloudWinMsg);
  },
  watch: {
    page(next) {
      if (next === '设置' && this.cloudAvailable) {
        void this.loadCloudConfig();
        void this.loadCloudSyncStatus();
      }
    }
  },
  methods: {
    cloudFormatSize,
    cloudKindLabel(kind) {
      return { webdav: 'WebDAV', gdrive: 'Google Drive', baidu: '百度网盘' }[kind] || kind;
    },
    async autoUploadCloudFile({
      relDir = '',
      filename = '',
      data,
      blob,
      mimeType = '',
      connectionId = '',
      providerKind = 'gdrive',
      overwrite = true,
      silent = true
    } = {}) {
      if (!this.cloudAvailable) {
        return { ok: true, skipped: true, reason: 'offline' };
      }
      if (
        providerKind === 'gdrive' &&
        !connectionId &&
        this.cloudLoaded &&
        !this.cloudConnections.some(c => c.kind === 'gdrive')
      ) {
        return { ok: true, skipped: true, reason: 'no_gdrive_connection' };
      }
      const cleanDir = cleanCloudRelDirClient(relDir);
      const cleanName = safeCloudFilename(filename, '');
      if (!cleanName) {
        return { ok: false, skipped: true, reason: 'invalid_filename' };
      }
      let body = data !== undefined ? data : blob;
      if (body == null) {
        return { ok: false, skipped: true, reason: 'empty_payload' };
      }
      let resolvedMime = String(mimeType || '').trim();
      if (
        typeof body === 'object' &&
        !(body instanceof Blob) &&
        !(body instanceof ArrayBuffer) &&
        !ArrayBuffer.isView(body)
      ) {
        body = JSON.stringify(body, null, 2);
        if (!resolvedMime) resolvedMime = 'application/json';
      }
      if (!resolvedMime && body instanceof Blob && body.type) {
        resolvedMime = body.type;
      }
      if (!resolvedMime) {
        resolvedMime = cloudMimeForName(cleanName);
      }
      const byteSize =
        body instanceof Blob ? body.size :
        body instanceof ArrayBuffer ? body.byteLength :
        ArrayBuffer.isView(body) ? body.byteLength :
        new TextEncoder().encode(String(body)).byteLength;
      if (byteSize > 128 * 1024 * 1024) {
        const errMsg = '自动上传文件超过 128 MB 上限：' + cleanName;
        if (!silent) this.notify?.(errMsg);
        return { ok: false, error: errMsg };
      }
      try {
        const meta = {
          connectionId: String(connectionId || ''),
          providerKind: String(providerKind || 'gdrive'),
          relDir: cleanDir,
          filename: cleanName,
          mimeType: resolvedMime,
          overwrite: Boolean(overwrite)
        };
        const resp = await fetch('library/cloud/auto-upload', {
          method: 'POST',
          headers: {
            'Content-Type': resolvedMime,
            'X-Biology-Library': '1',
            'X-Cloud-Meta': encodeCloudMetaHeader(meta)
          },
          body
        });
        if (!resp.ok) {
          const errText = (await resp.text()).trim() || '云盘自动上传失败';
          console.warn('[biofolio] auto-upload failed:', cleanName, resp.status, errText);
          if (!silent) this.notify?.('云盘自动上传失败：' + errText);
          return { ok: false, status: resp.status, error: errText };
        }
        const json = await resp.json();
        return { ok: true, skipped: Boolean(json.skipped), ...json };
      } catch (err) {
        const errMsg = err?.message || '网络错误';
        console.warn('[biofolio] auto-upload error:', cleanName, errMsg);
        if (!silent) this.notify?.('云盘自动上传失败：' + errMsg);
        return { ok: false, error: errMsg };
      }
    },
    formatCloudSyncTime(iso) {
      if (!iso) return '—';
      const d = new Date(iso);
      if (Number.isNaN(d.getTime())) return String(iso);
      const pad = n => String(n).padStart(2, '0');
      return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
    },
    applyCloudSyncStatus(data) {
      if (!data || typeof data !== 'object') return;
      this.cloudSyncStatus = {
        ...this.cloudSyncStatus,
        ...data,
        local: { stateVersion: 0, recordCount: 0, syncVersion: 0, ...(data.local || {}) },
        remote: {
          checked: false,
          checkedAt: '',
          exists: false,
          empty: false,
          syncVersion: 0,
          updatedAt: '',
          recordCount: 0,
          shardCount: 0,
          error: '',
          ...(data.remote || {})
        }
      };
      this.cloudSyncLoaded = true;
      if (
        !data.configured ||
        data.status === 'error' ||
        data.ok === false ||
        Boolean(data.remote?.error) ||
        (data.status !== 'remote_empty_local_populated' && data.remote?.exists && !data.remote?.empty)
      ) {
        this.cloudSyncEmptyRemoteAlert = false;
      }
    },
    stopCloudSyncTimers() {
      if (this._cloudSyncDebounceTimer) {
        clearTimeout(this._cloudSyncDebounceTimer);
        this._cloudSyncDebounceTimer = null;
      }
      if (this._cloudSyncPollTimer) {
        clearTimeout(this._cloudSyncPollTimer);
        this._cloudSyncPollTimer = null;
      }
    },
    scheduleCloudSyncPoll() {
      if (this._cloudSyncUnmounted || !this.cloudAvailable || this._cloudSyncPollTimer || (this._cloudSyncPollCount || 0) >= 24) {
        return;
      }
      this._cloudSyncPollCount = (this._cloudSyncPollCount || 0) + 1;
      this._cloudSyncPollTimer = setTimeout(() => {
        this._cloudSyncPollTimer = null;
        if (!this._cloudSyncUnmounted) {
          void this.loadCloudSyncStatus({ schedulePoll: true });
        }
      }, 2500);
    },
    onLibraryMutationSync() {
      if (this._cloudSyncUnmounted || !this.cloudAvailable) return;
      this._cloudSyncPollCount = 0;
      if (this._cloudSyncDebounceTimer) clearTimeout(this._cloudSyncDebounceTimer);
      this._cloudSyncDebounceTimer = setTimeout(() => {
        this._cloudSyncDebounceTimer = null;
        if (!this._cloudSyncUnmounted) {
          void this.loadCloudSyncStatus({ schedulePoll: true });
        }
      }, 600);
    },
    async loadCloudSyncStatus({ schedulePoll = true } = {}) {
      if (!this.cloudAvailable) return null;
      if (this._cloudSyncStatusInFlight) return this._cloudSyncStatusInFlight;
      this._cloudSyncStatusInFlight = (async () => {
        try {
          const resp = await fetch('library/cloud/sync/status', { cache: 'no-store' });
          if (!resp.ok) return null;
          const data = await resp.json();
          this.applyCloudSyncStatus(data);
          if (schedulePoll && data.configured && (data.syncing || data.dirty)) {
            this.scheduleCloudSyncPoll();
          } else if (!data.syncing && !data.dirty) {
            this._cloudSyncPollCount = 0;
          }
          return data;
        } catch {
          return null;
        } finally {
          this._cloudSyncStatusInFlight = null;
        }
      })();
      return this._cloudSyncStatusInFlight;
    },
    async checkCloudSyncStatus() {
      if (!this.cloudAvailable || !this.admin || this.cloudSyncBusy) return null;
      this.cloudSyncBusy = true;
      this.cloudSyncNotice = '';
      try {
        const resp = await libraryPost('cloud/sync/check', {});
        const data = await resp.json();
        this.applyCloudSyncStatus(data);
        if (!data.configured) {
          this.cloudSyncNotice = '尚未绑定管理员 Google Drive 连接。';
        } else if (data.status === 'error' || data.ok === false || data.remote?.error) {
          this.cloudSyncNotice = data.remote?.error || data.lastSyncError || '远端同步库检查异常';
        } else if (data.status === 'remote_empty_local_populated') {
          this.cloudSyncEmptyRemoteAlert = true;
          this.cloudSyncNotice = '远端 Google Drive「同步库/」为空，已保护本地数据不被覆盖。';
        } else {
          this.cloudSyncNotice = '已检查远端同步库状态：' + this.cloudSyncStatusLabel + '。';
        }
        return data;
      } catch (e) {
        this.cloudSyncNotice = '检查失败：' + e.message;
        return null;
      } finally {
        this.cloudSyncBusy = false;
      }
    },
    async pushCloudSyncNow() {
      if (!this.cloudAvailable || !this.admin || this.cloudSyncBusy) return null;
      this.cloudSyncBusy = true;
      this.cloudSyncNotice = '正在将本地变更同步到 Google Drive…';
      try {
        const resp = await libraryPost('cloud/sync/push', {});
        const data = await resp.json();
        this.applyCloudSyncStatus(data);
        this.cloudSyncEmptyRemoteAlert = false;
        this.cloudSyncNotice = '已同步到 Google Drive。';
        this.notify('已同步到 Google Drive');
        return data;
      } catch (e) {
        await this.loadCloudSyncStatus({ schedulePoll: false });
        this.cloudSyncNotice = '同步失败：' + e.message;
        return null;
      } finally {
        this.cloudSyncBusy = false;
      }
    },
    async startGoogleDriveSyncRestore() {
      if (!this.cloudAvailable || !this.admin || this.cloudSyncBusy) return;
      this.cloudSyncBusy = true;
      this.cloudSyncNotice = '正在检查远端 Google Drive 同步库…';
      this.cloudSyncEmptyRemoteAlert = false;
      this.cloudSyncRestoreError = '';
      try {
        const resp = await libraryPost('cloud/sync/check', {});
        const st = await resp.json();
        this.applyCloudSyncStatus(st);
        if (!st.configured) {
          this.cloudSyncNotice = '尚未绑定管理员 Google Drive 连接，请先在上方「我的云盘」中绑定。';
          return;
        }
        if (st.status === 'error' || st.ok === false || st.remote?.error) {
          this.cloudSyncNotice = '无法检查远端同步库：' + (st.remote?.error || st.lastSyncError || '请检查网络或代理后重试');
          return;
        }
        if (st.remote?.empty || !st.remote?.exists || st.status === 'remote_empty_local_populated') {
          this.cloudSyncEmptyRemoteAlert = true;
          this.cloudSyncNotice = '远端 Google Drive「同步库/」为空，已阻止恢复以保护本地数据。';
          return;
        }
        this.cloudSyncNotice = '';
        this.cloudSyncRestoreMode = 'merge';
        this.$refs.cloudSyncRestoreDialog?.showModal();
      } catch (e) {
        this.cloudSyncNotice = '检查远端同步库失败：' + e.message;
      } finally {
        this.cloudSyncBusy = false;
      }
    },
    async confirmGoogleDriveSyncRestore() {
      if (!this.cloudAvailable || !this.admin || this.cloudSyncBusy) return;
      this.cloudSyncBusy = true;
      this.cloudSyncRestoreError = '';
      try {
        const resp = await libraryPost('cloud/sync/restore', {
          connectionId: this.cloudSyncStatus.connectionId || '',
          mode: this.cloudSyncRestoreMode || 'merge'
        });
        const data = await resp.json();
        this.applyCloudSyncStatus(data);
        this.$refs.cloudSyncRestoreDialog?.close();
        this.cloudSyncNotice = '已从 Google Drive 同步恢复，正在刷新页面…';
        this.notify('已从 Google Drive 同步恢复');
        if (typeof location !== 'undefined' && typeof location.reload === 'function') {
          setTimeout(() => location.reload(), 300);
        }
      } catch (e) {
        this.cloudSyncRestoreError = e.message || '恢复失败';
        this.cloudSyncNotice = this.cloudSyncRestoreError;
      } finally {
        this.cloudSyncBusy = false;
      }
    },
    goToCloudSyncSettings() {
      this.go('设置');
      this.$nextTick(() => {
        document.getElementById('google-drive-sync-card')?.scrollIntoView({ behavior: 'smooth', block: 'start' });
      });
    },
    async loadCloudConfig() {
      if (!this.cloudAvailable) return;
      try {
        const res = await fetch('library/cloud/config', { cache: 'no-store' });
        if (!res.ok) return;
        const data = await res.json();
        this.cloudConnections = data.connections || [];
        if (data.shared) this.cloudShared = { ...this.cloudShared, ...data.shared };
        if (data.adminConfig) this.applyCloudAdminView(data.adminConfig);
        if (data.autoBackupState) this.cloudAutoBackupState = data.autoBackupState;
        this.cloudLoaded = true;
      } catch {}
    },
    applyCloudAdminView(cfg) {
      this.cloudAdminForm = {
        proxyUrl: cfg.proxyUrl || '',
        proxyScope: cfg.proxyScope === 'all' ? 'all' : 'google',
        allowPrivateWebdav: Boolean(cfg.allowPrivateWebdav),
        prefixesText: (cfg.allowedWebdavPrefixes || []).join('\n'),
        googleClientId: cfg.googleClientId || '',
        googleClientSecret: '',
        googleHasSecret: Boolean(cfg.googleHasSecret),
        googleRedirectUri: cfg.googleRedirectUri || '',
        baiduAppKey: cfg.baiduAppKey || '',
        baiduSecretKey: '',
        baiduHasSecret: Boolean(cfg.baiduHasSecret),
        baiduAppDir: cfg.baiduAppDir || '/apps/Biofolio',
        envProxy: cfg.envProxy || '',
        envAllowPrivateWebdav: Boolean(cfg.envAllowPrivateWebdav),
        envGoogleReady: Boolean(cfg.envGoogleReady),
        envBaiduReady: Boolean(cfg.envBaiduReady)
      };
      this.cloudAutoBackupForm = {
        enabled: Boolean(cfg.autoBackupEnabled),
        connectionId: cfg.autoBackupConnectionId || (this.cloudDefaultConnection?.id || ''),
        dir: cfg.autoBackupDir || '',
        hour: Number.isInteger(cfg.autoBackupHour) ? cfg.autoBackupHour : 3
      };
    },
    async saveCloudAutoBackupSettings() {
      if (this.cloudAutoBackupBusy) return;
      this.cloudAutoBackupBusy = true;
      this.cloudAutoBackupNotice = '';
      try {
        const resp = await libraryPost('cloud/auto-backup', {
          enabled: Boolean(this.cloudAutoBackupForm.enabled),
          connectionId: this.cloudAutoBackupForm.connectionId,
          dir: this.cloudAutoBackupForm.dir.trim(),
          hour: Number(this.cloudAutoBackupForm.hour)
        });
        const data = await resp.json();
        if (data.adminConfig) this.applyCloudAdminView(data.adminConfig);
        if (data.autoBackupState) this.cloudAutoBackupState = data.autoBackupState;
        this.cloudAutoBackupNotice = this.cloudAutoBackupForm.enabled ? '已开启每日自动云盘备份。' : '已关闭自动云盘备份。';
      } catch (e) {
        this.cloudAutoBackupNotice = e.message || '保存失败';
      } finally {
        this.cloudAutoBackupBusy = false;
      }
    },
    async runCloudAutoBackupNow() {
      if (this.cloudAutoBackupBusy) return;
      this.cloudAutoBackupBusy = true;
      this.cloudAutoBackupNotice = '正在备份并上传整库 JSON 与离线题库 HTML…';
      try {
        const resp = await libraryPost('cloud/auto-backup-run', { force: true });
        const data = await resp.json();
        if (data.autoBackupState) this.cloudAutoBackupState = data.autoBackupState;
        this.cloudAutoBackupNotice = data.autoBackupState?.lastMessage || '云盘备份完成。';
        this.notify(this.cloudAutoBackupNotice);
      } catch (e) {
        this.cloudAutoBackupNotice = '备份失败：' + e.message;
      } finally {
        this.cloudAutoBackupBusy = false;
      }
    },
    async saveCloudAdminSettings(opts = {}) {
      if (this.cloudAdminBusy) return;
      this.cloudAdminBusy = true;
      this.cloudAdminNotice = '';
      try {
        const prefixes = this.cloudAdminForm.prefixesText
          .split(/\r?\n/)
          .map(s => s.trim())
          .filter(Boolean);
        const body = {
          proxyUrl: this.cloudAdminForm.proxyUrl.trim(),
          proxyScope: this.cloudAdminForm.proxyScope,
          allowPrivateWebdav: Boolean(this.cloudAdminForm.allowPrivateWebdav),
          allowedWebdavPrefixes: prefixes,
          googleClientId: this.cloudAdminForm.googleClientId.trim(),
          googleClientSecret: this.cloudAdminForm.googleClientSecret.trim(),
          clearGoogleSecret: Boolean(opts.clearGoogleSecret),
          googleRedirectUri: this.cloudAdminForm.googleRedirectUri.trim(),
          baiduAppKey: this.cloudAdminForm.baiduAppKey.trim(),
          baiduSecretKey: this.cloudAdminForm.baiduSecretKey.trim(),
          clearBaiduSecret: Boolean(opts.clearBaiduSecret),
          baiduAppDir: this.cloudAdminForm.baiduAppDir.trim() || '/apps/Biofolio'
        };
        const resp = await libraryPost('cloud/admin-settings', body);
        const data = await resp.json();
        if (data.adminConfig) this.applyCloudAdminView(data.adminConfig);
        if (data.shared) this.cloudShared = { ...this.cloudShared, ...data.shared };
        this.cloudAdminNotice = '云盘接入服务设置已保存。';
      } catch (e) {
        this.cloudAdminNotice = e.message || '保存失败';
      } finally {
        this.cloudAdminBusy = false;
      }
    },
    openAddCloud(tab = 'webdav') {
      this.cloudAddTab = tab;
      this.cloudShowAdd = true;
      this.cloudNotice = '';
      this.cloudWebdavNotice = '';
      this.cloudGoogleNotice = '';
      this.cloudBaiduNotice = '';
      this.cloudWebdavForm = { id: '', name: '', baseUrl: '', username: '', password: '', rootDir: '', isDefault: !this.cloudConnections.length, useProxy: false };
    },
    startEditCloudMeta(conn) {
      this.cloudEditMetaId = conn.id;
      this.cloudEditMetaForm = {
        name: conn.name,
        rootDir: conn.rootDir || '',
        isDefault: Boolean(conn.isDefault),
        useProxy: Boolean(conn.useProxy)
      };
      this.cloudNotice = '';
    },
    async saveCloudMeta(conn) {
      if (this.cloudBusyId) return;
      this.cloudBusyId = conn.id;
      this.cloudNotice = '';
      try {
        const resp = await libraryPost('cloud/connections', {
          action: 'update_meta',
          id: conn.id,
          name: this.cloudEditMetaForm.name,
          rootDir: this.cloudEditMetaForm.rootDir,
          isDefault: this.cloudEditMetaForm.isDefault,
          useProxy: this.cloudEditMetaForm.useProxy
        });
        const data = await resp.json();
        this.cloudConnections = data.connections || [];
        this.cloudEditMetaId = '';
        this.cloudNotice = '已更新云盘设置。';
        void this.loadCloudSyncStatus();
      } catch (e) {
        this.cloudNotice = e.message;
      } finally {
        this.cloudBusyId = '';
      }
    },
    async setCloudDefault(conn) {
      if (this.cloudBusyId || conn.isDefault) return;
      this.cloudBusyId = conn.id;
      this.cloudNotice = '';
      try {
        const resp = await libraryPost('cloud/connections', {
          action: 'update_meta',
          id: conn.id,
          name: conn.name,
          rootDir: conn.rootDir || '',
          isDefault: true,
          useProxy: Boolean(conn.useProxy)
        });
        const data = await resp.json();
        this.cloudConnections = data.connections || [];
        this.cloudNotice = '已将「' + conn.name + '」设为默认云盘。';
        void this.loadCloudSyncStatus();
      } catch (e) {
        this.cloudNotice = e.message;
      } finally {
        this.cloudBusyId = '';
      }
    },
    async testCloudConnection(conn) {
      if (this.cloudBusyId) return;
      this.cloudBusyId = conn.id;
      this.cloudNotice = '';
      try {
        const resp = await libraryPost('cloud/connections', { action: 'test', id: conn.id });
        const data = await resp.json();
        if (data.connections) this.cloudConnections = data.connections;
        this.cloudNotice = '「' + conn.name + '」连接正常' + (data.label ? '（' + data.label + '）' : '') + '。';
      } catch (e) {
        this.cloudNotice = '「' + conn.name + '」连接失败：' + e.message;
      } finally {
        this.cloudBusyId = '';
      }
    },
    async removeCloudConnection(conn) {
      if (this.cloudBusyId) return;
      if (!confirm('解除绑定云盘「' + conn.name + '」？不会删除云盘上的文件。')) return;
      this.cloudBusyId = conn.id;
      this.cloudNotice = '';
      try {
        const resp = await libraryPost('cloud/connections', { action: 'delete', id: conn.id });
        const data = await resp.json();
        this.cloudConnections = data.connections || [];
        this.cloudNotice = '已解绑「' + conn.name + '」。';
        void this.loadCloudSyncStatus();
      } catch (e) {
        this.cloudNotice = e.message;
      } finally {
        this.cloudBusyId = '';
      }
    },
    async saveWebdavConnection() {
      if (this.cloudWebdavBusy) return;
      this.cloudWebdavBusy = true;
      this.cloudWebdavNotice = '正在测试并保存 WebDAV 连接…';
      try {
        const resp = await libraryPost('cloud/connections', {
          action: 'save_webdav',
          ...this.cloudWebdavForm
        });
        const data = await resp.json();
        this.cloudConnections = data.connections || [];
        this.cloudShowAdd = false;
        this.cloudWebdavNotice = '';
        this.cloudNotice = '已绑定 WebDAV「' + this.cloudWebdavForm.name + '」。';
      } catch (e) {
        this.cloudWebdavNotice = e.message;
      } finally {
        this.cloudWebdavBusy = false;
      }
    },
    async startGoogleOAuth() {
      if (this.cloudGoogleBusy) return;
      this.cloudGoogleBusy = true;
      this.cloudGoogleNotice = '';
      this.cloudGoogleAuthUrl = '';
      this.cloudGoogleState = '';
      try {
        const resp = await libraryPost('cloud/google/start', {
          mode: this.cloudGoogleMode,
          name: this.cloudGoogleName.trim() || '我的 Google Drive',
          rootDir: this.cloudGoogleRootDir.trim(),
          customClientId: this.cloudGoogleShowCustom ? this.cloudGoogleCustomId.trim() : '',
          customClientSecret: this.cloudGoogleShowCustom ? this.cloudGoogleCustomSecret.trim() : ''
        });
        const data = await resp.json();
        this.cloudGoogleState = data.state;
        this.cloudGoogleAuthUrl = data.authUrl;
        this.cloudGoogleRedirectUri = data.redirectUri;
        const win = window.open(data.authUrl, 'biofolio_google_oauth', 'width=560,height=700');
        if (this.cloudGoogleMode === 'web') {
          this.cloudGoogleNotice = win
            ? '已打开 Google 授权窗口，请在弹出的页面登录并允许访问。'
            : '浏览器拦截了弹窗，请点击下方「打开 Google 授权页面」链接完成授权。';
        } else {
          this.cloudGoogleNotice = '请在打开的 Google 页面完成授权；当页面跳转到「无法访问」的 localhost:1 页面时，完整复制浏览器地址栏链接粘贴到下方即可。';
        }
      } catch (e) {
        this.cloudGoogleNotice = e.message;
      } finally {
        this.cloudGoogleBusy = false;
      }
    },
    async completeGoogleManualOAuth() {
      if (this.cloudGoogleBusy || !this.cloudGoogleState || !this.cloudGoogleManualInput.trim()) return;
      this.cloudGoogleBusy = true;
      this.cloudGoogleNotice = '正在换取访问令牌…';
      try {
        const resp = await libraryPost('cloud/google/complete', {
          state: this.cloudGoogleState,
          input: this.cloudGoogleManualInput.trim()
        });
        const data = await resp.json();
        this.cloudConnections = data.connections || [];
        this.cloudGoogleState = '';
        this.cloudGoogleAuthUrl = '';
        this.cloudGoogleManualInput = '';
        this.cloudGoogleRootDir = 'Biofolio';
        this.cloudShowAdd = false;
        this.cloudGoogleNotice = '';
        this.cloudNotice = '已连接 Google Drive「' + (data.connection?.name || '') + '」。';
        void this.loadCloudSyncStatus();
      } catch (e) {
        this.cloudGoogleNotice = e.message;
      } finally {
        this.cloudGoogleBusy = false;
      }
    },
    stopBaiduPolling() {
      if (this._cloudBaiduTimer) {
        clearTimeout(this._cloudBaiduTimer);
        this._cloudBaiduTimer = null;
      }
      this.cloudBaiduSessionId = '';
    },
    async startBaiduQRAuth() {
      if (this.cloudBaiduBusy) return;
      this.stopBaiduPolling();
      this.cloudBaiduBusy = true;
      this.cloudBaiduNotice = '正在获取百度网盘登录二维码…';
      this.cloudBaiduQR = '';
      this.cloudBaiduUserCode = '';
      this.cloudBaiduVerifyUrl = '';
      try {
        const resp = await libraryPost('cloud/baidu/start', {
          name: this.cloudBaiduName.trim() || '我的百度网盘',
          rootDir: this.cloudBaiduRootDir.trim(),
          useProxy: Boolean(this.cloudBaiduUseProxy),
          customAppKey: this.cloudBaiduShowCustom ? this.cloudBaiduCustomKey.trim() : '',
          customSecretKey: this.cloudBaiduShowCustom ? this.cloudBaiduCustomSecret.trim() : '',
          customAppDir: this.cloudBaiduShowCustom ? this.cloudBaiduCustomDir.trim() : ''
        });
        const data = await resp.json();
        this.cloudBaiduSessionId = data.sessionId;
        this.cloudBaiduQR = data.qrDataUrl || '';
        this.cloudBaiduUserCode = data.userCode || '';
        this.cloudBaiduVerifyUrl = data.verificationUrl || '';
        this.cloudBaiduAppDir = data.appDir || '/apps/Biofolio';
        this.cloudBaiduNotice = '请用手机「百度网盘 App」扫码确认授权，授权后会自动绑定。';
        const intervalMs = Math.max(3, Number(data.interval) || 5) * 1000;
        const pollLoop = async () => {
          const sid = this.cloudBaiduSessionId;
          if (!sid) return;
          try {
            const pResp = await libraryPost('cloud/baidu/poll', { sessionId: sid });
            const pData = await pResp.json();
            if (sid !== this.cloudBaiduSessionId) return;
            if (pData.status === 'connected') {
              this.stopBaiduPolling();
              this.cloudBaiduQR = '';
              this.cloudConnections = pData.connections || [];
              this.cloudShowAdd = false;
              this.cloudBaiduNotice = '';
              this.cloudNotice = '已连接百度网盘' + (pData.label ? '（' + pData.label + '）' : '') + '。';
              return;
            }
            this._cloudBaiduTimer = setTimeout(pollLoop, intervalMs);
          } catch (err) {
            if (sid === this.cloudBaiduSessionId) {
              this.stopBaiduPolling();
              this.cloudBaiduNotice = err.message;
            }
          }
        };
        this._cloudBaiduTimer = setTimeout(pollLoop, intervalMs);
      } catch (e) {
        this.cloudBaiduNotice = e.message;
      } finally {
        this.cloudBaiduBusy = false;
      }
    },
    cloudEntrySelectable(entry) {
      if (!entry || entry.isDir) return false;
      if (!this.cloudDialogAccept?.length) return true;
      const lower = String(entry.name || '').toLowerCase();
      const exp = String(entry.exportAs || '').toLowerCase();
      return this.cloudDialogAccept.some(ext => lower.endsWith(ext) || (exp && exp === ext));
    },
    async listCloudDir(dir = '', crumbStack = null) {
      if (!this.cloudDialogConnId) return;
      this.cloudDialogLoading = true;
      this.cloudDialogError = '';
      this.cloudDialogSelected = [];
      try {
        const resp = await libraryPost('cloud/list', {
          connectionId: this.cloudDialogConnId,
          dir
        });
        const data = await resp.json();
        this.cloudDialogDir = data.dir || '';
        this.cloudDialogDirName = data.dirName || '根目录';
        this.cloudDialogParentDir = data.parentDir || '';
        this.cloudDialogEntries = data.entries || [];
        if (Array.isArray(data.crumbs) && data.crumbs.length) {
          this.cloudDialogCrumbs = data.crumbs;
        } else if (Array.isArray(crumbStack)) {
          this.cloudDialogCrumbs = crumbStack;
        } else if (!this.cloudDialogCrumbs.length || dir === '') {
          this.cloudDialogCrumbs = [{ id: '', name: this.cloudDialogDirName }];
        }
      } catch (e) {
        this.cloudDialogError = e.message;
      } finally {
        this.cloudDialogLoading = false;
      }
    },
    switchCloudDialogConnection(connId) {
      this.cloudDialogConnId = connId;
      this.cloudDialogCrumbs = [];
      void this.listCloudDir('');
    },
    openCloudFolder(entry) {
      const nextCrumbs = [...this.cloudDialogCrumbs, { id: entry.id, name: entry.name }];
      void this.listCloudDir(entry.id, nextCrumbs);
    },
    jumpCloudCrumb(index) {
      const target = this.cloudDialogCrumbs[index];
      if (!target) return;
      void this.listCloudDir(target.id, this.cloudDialogCrumbs.slice(0, index + 1));
    },
    cloudGoUp() {
      if (this.cloudDialogCrumbs.length > 1) {
        const nextCrumbs = this.cloudDialogCrumbs.slice(0, -1);
        void this.listCloudDir(nextCrumbs[nextCrumbs.length - 1].id, nextCrumbs);
        return;
      }
      if (this.cloudDialogParentDir || this.cloudDialogDir !== '') {
        void this.listCloudDir(this.cloudDialogParentDir);
      }
    },
    toggleCloudPick(entry) {
      if (!this.cloudEntrySelectable(entry)) return;
      if (this.cloudDialogMultiple) {
        if (this.cloudDialogSelected.includes(entry.id)) {
          this.cloudDialogSelected = this.cloudDialogSelected.filter(x => x !== entry.id);
        } else {
          this.cloudDialogSelected = [...this.cloudDialogSelected, entry.id];
        }
      } else {
        this.cloudDialogSelected = [entry.id];
      }
    },
    async createCloudFolder() {
      const name = this.cloudDialogNewFolder.trim();
      if (!name || this.cloudDialogLoading) return;
      this.cloudDialogLoading = true;
      this.cloudDialogError = '';
      try {
        await libraryPost('cloud/mkdir', {
          connectionId: this.cloudDialogConnId,
          parentDir: this.cloudDialogDir,
          name
        });
        this.cloudDialogNewFolder = '';
        this.cloudDialogShowNewFolder = false;
        await this.listCloudDir(this.cloudDialogDir, this.cloudDialogCrumbs);
      } catch (e) {
        this.cloudDialogError = e.message;
        this.cloudDialogLoading = false;
      }
    },
    async openCloudDialogBase({ mode, title, accept = [], multiple = false, filename = '', backupKind = 'json' }) {
      if (!this.cloudLoaded) await this.loadCloudConfig();
      this.cloudDialogMode = mode;
      this.cloudDialogTitle = title;
      this.cloudDialogAccept = accept.map(s => s.toLowerCase());
      this.cloudDialogMultiple = multiple;
      this.cloudDialogSelected = [];
      this.cloudDialogFilename = filename;
      this.cloudDialogOverwrite = false;
      this.cloudDialogShowNewFolder = false;
      this.cloudDialogNewFolder = '';
      this.cloudDialogError = '';
      this.cloudDialogEntries = [];
      this.cloudDialogCrumbs = [];
      this.cloudDialogTransferBusy = false;
      this.cloudDialogBackupKind = backupKind;
      const def = this.cloudDefaultConnection;
      this.cloudDialogConnId = def ? def.id : '';
      this.$refs.cloudDialog?.showModal();
      if (this.cloudDialogConnId) {
        await this.listCloudDir('');
      }
    },
    pickCloudFiles({ title = '从我的云盘选择文件', accept = [], multiple = false } = {}) {
      if (!this.cloudAvailable) return Promise.resolve(null);
      return new Promise(resolve => {
        this._cloudResolve = resolve;
        this._cloudPendingBlob = null;
        void this.openCloudDialogBase({ mode: 'pick', title, accept, multiple });
      });
    },
    saveBlobToCloud({ blob, filename, mimeType = '', title = '保存到我的云盘' }) {
      if (!this.cloudAvailable) return Promise.resolve(null);
      return new Promise(resolve => {
        this._cloudResolve = resolve;
        this._cloudPendingBlob = { blob, mimeType: mimeType || blob.type || cloudMimeForName(filename) };
        void this.openCloudDialogBase({ mode: 'save', title, filename });
      });
    },
    async exportBackupToCloud(kind = 'json') {
      if (!this.cloudAvailable || !this.admin) return;
      if (!this.cloudLoaded) await this.loadCloudConfig();
      this._cloudResolve = null;
      this._cloudPendingBlob = null;
      await this.openCloudDialogBase({
        mode: 'backup-dir',
        title: kind === 'offline' ? '选择保存「离线题库」的云盘目录' : '选择保存「整库备份」的云盘目录',
        backupKind: kind
      });
    },
    async restoreBackupFromCloud() {
      if (!this.cloudAvailable || !this.admin || this.libraryServer?.version !== 0) return;
      if (!this.cloudLoaded) await this.loadCloudConfig();
      this._cloudResolve = null;
      this._cloudPendingBlob = null;
      await this.openCloudDialogBase({
        mode: 'backup-restore',
        title: '从云盘选择整库备份（.json）恢复',
        accept: ['.json'],
        multiple: false
      });
    },
    goToCloudSettings() {
      this.cancelCloudDialog();
      this.go('设置');
      this.$nextTick(() => {
        this.openAddCloud('webdav');
        document.getElementById('my-cloud-settings')?.scrollIntoView({ behavior: 'smooth', block: 'start' });
      });
    },
    cancelCloudDialog() {
      const resolve = this._cloudResolve;
      this._cloudResolve = null;
      this._cloudPendingBlob = null;
      if (this.$refs.cloudDialog?.open) this.$refs.cloudDialog.close();
      if (resolve) resolve(null);
    },
    onCloudDialogClose() {
      const resolve = this._cloudResolve;
      this._cloudResolve = null;
      this._cloudPendingBlob = null;
      if (resolve) resolve(null);
    },
    async confirmCloudDialog() {
      if (this.cloudDialogTransferBusy || !this.cloudDialogConnId) return;
      this.cloudDialogError = '';
      if (this.cloudDialogMode === 'pick') {
        if (!this.cloudDialogSelected.length) return;
        this.cloudDialogTransferBusy = true;
        try {
          const files = [];
          for (const fileId of this.cloudDialogSelected) {
            const entry = this.cloudDialogEntries.find(e => e.id === fileId);
            const resp = await fetch('library/cloud/download', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json', 'X-Biology-Library': '1' },
              body: JSON.stringify({ connectionId: this.cloudDialogConnId, fileId })
            });
            if (!resp.ok) throw Error((await resp.text()).trim() || '下载云盘文件失败');
            const hdrName = resp.headers.get('X-Cloud-Filename');
            let fname = hdrName ? decodeURIComponent(hdrName) : (entry?.name || '云盘文件');
            if (entry?.exportAs && !fname.toLowerCase().endsWith(entry.exportAs)) fname += entry.exportAs;
            const blob = await resp.blob();
            const type = cloudMimeForName(fname, blob.type || 'application/octet-stream');
            files.push(new File([blob], fname, { type }));
          }
          const resolve = this._cloudResolve;
          this._cloudResolve = null;
          this.$refs.cloudDialog?.close();
          if (resolve) resolve(files);
        } catch (e) {
          this.cloudDialogError = e.message;
        } finally {
          this.cloudDialogTransferBusy = false;
        }
        return;
      }

      if (this.cloudDialogMode === 'save') {
        const filename = this.cloudDialogFilename.trim();
        if (!filename || !this._cloudPendingBlob?.blob) {
          this.cloudDialogError = '请填写保存文件名';
          return;
        }
        this.cloudDialogTransferBusy = true;
        try {
          const meta = {
            connectionId: this.cloudDialogConnId,
            dir: this.cloudDialogDir,
            filename,
            mimeType: this._cloudPendingBlob.mimeType || cloudMimeForName(filename),
            overwrite: Boolean(this.cloudDialogOverwrite)
          };
          const resp = await fetch('library/cloud/upload', {
            method: 'POST',
            headers: {
              'Content-Type': meta.mimeType,
              'X-Biology-Library': '1',
              'X-Cloud-Meta': encodeCloudMetaHeader(meta)
            },
            body: this._cloudPendingBlob.blob
          });
          if (!resp.ok) throw Error((await resp.text()).trim() || '保存到云盘失败');
          const data = await resp.json();
          const savedName = data.entry?.name || filename;
          const resolve = this._cloudResolve;
          this._cloudResolve = null;
          this._cloudPendingBlob = null;
          this.$refs.cloudDialog?.close();
          this.notify('已保存到云盘：' + savedName);
          if (resolve) resolve(data);
        } catch (e) {
          this.cloudDialogError = e.message;
        } finally {
          this.cloudDialogTransferBusy = false;
        }
        return;
      }

      if (this.cloudDialogMode === 'backup-dir') {
        this.cloudDialogTransferBusy = true;
        try {
          const resp = await libraryPost('cloud/backup-export', {
            connectionId: this.cloudDialogConnId,
            dir: this.cloudDialogDir,
            kind: this.cloudDialogBackupKind,
            overwrite: Boolean(this.cloudDialogOverwrite)
          });
          const data = await resp.json();
          const savedName = data.entry?.name || '备份文件';
          this.$refs.cloudDialog?.close();
          this.backupNotice = '已上传到云盘：' + savedName;
          this.notify(this.backupNotice);
        } catch (e) {
          this.cloudDialogError = e.message;
        } finally {
          this.cloudDialogTransferBusy = false;
        }
        return;
      }

      if (this.cloudDialogMode === 'backup-restore') {
        if (!this.cloudDialogSelected.length) return;
        const fileId = this.cloudDialogSelected[0];
        if (!confirm('确认从选中的云盘备份恢复整个题库？恢复后页面将自动刷新。')) return;
        this.cloudDialogTransferBusy = true;
        try {
          await libraryPost('cloud/backup-restore', {
            connectionId: this.cloudDialogConnId,
            fileId
          });
          this.$refs.cloudDialog?.close();
          location.reload();
        } catch (e) {
          this.cloudDialogError = e.message;
        } finally {
          this.cloudDialogTransferBusy = false;
        }
      }
    }
  }
};
