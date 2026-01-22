/* ============================================================
   NEURAL FIELD (ALWAYS-ON BACKGROUND CANVAS)
   ============================================================ */

const canvas = document.getElementById('brain');
const ctx = canvas.getContext('2d', { alpha: true });

function resize() {
  const dpr = Math.max(1, Math.min(2, window.devicePixelRatio || 1));
  canvas.width = Math.floor(window.innerWidth * dpr);
  canvas.height = Math.floor(window.innerHeight * dpr);
  canvas.style.width = window.innerWidth + 'px';
  canvas.style.height = window.innerHeight + 'px';
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);

  const bootfx = document.getElementById('bootfx');
  if (bootfx) {
    const bctx = bootfx.getContext('2d');
    bootfx.width = Math.floor(window.innerWidth * dpr);
    bootfx.height = Math.floor(window.innerHeight * dpr);
    bootfx.style.width = window.innerWidth + 'px';
    bootfx.style.height = window.innerHeight + 'px';
    bctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  }
}
window.addEventListener('resize', resize);
resize();

function rand(a, b) { return a + Math.random() * (b - a); }

/* ===== Neural system state ===== */

let bgRunning = true;
let activity = 0;
let activityVel = 0;
let lastNow = performance.now();

const NODES = [];
const PULSES = [];

const NODE_COUNT = Math.floor(Math.min(110, Math.max(70, window.innerWidth / 16)));
const LINK_DIST = 140;
const PULSE_SPAWN_BASE = 0.012;

function initNodes() {
  NODES.length = 0;
  for (let i = 0; i < NODE_COUNT; i++) {
    NODES.push({
      x: rand(0, window.innerWidth),
      y: rand(0, window.innerHeight),
      vx: rand(-0.12, 0.12),
      vy: rand(-0.12, 0.12),
      r: rand(1.1, 2.1),
      phase: rand(0, Math.PI * 2)
    });
  }
}
initNodes();

function bumpActivity(amount) {
  activityVel += amount;
  activity = Math.min(1, activity + amount * 0.35);
}

function spawnPulse(x, y, energy) {
  PULSES.push({
    x, y,
    r: 0,
    speed: rand(1.6, 3.2) + energy * 2.0,
    life: rand(90, 140),
    alpha: 0.25 + energy * 0.35
  });
  if (PULSES.length > 28) PULSES.shift();
}

/* ===== Neural render ===== */

function drawVignette() {
  const g = ctx.createRadialGradient(
      window.innerWidth / 2, window.innerHeight / 2, 120,
      window.innerWidth / 2, window.innerHeight / 2,
      Math.max(window.innerWidth, window.innerHeight) * 0.75
  );
  g.addColorStop(0, 'rgba(2,6,23,0.02)');
  g.addColorStop(1, 'rgba(0,0,0,0.46)');
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, window.innerWidth, window.innerHeight);
}

function renderNeural(now) {
  if (!bgRunning) return;

  const dt = Math.min(32, now - lastNow);
  lastNow = now;

  activityVel *= 0.88;
  activity = Math.max(0, Math.min(1, activity * 0.975 + activityVel * 0.02));

  ctx.fillStyle = 'rgba(0,0,0,0.18)';
  ctx.fillRect(0, 0, window.innerWidth, window.innerHeight);

  const t = now / 1000;
  const energy = activity;
  const drift = 0.10 + energy * 0.22;

  for (const n of NODES) {
    n.x += n.vx * dt * drift;
    n.y += n.vy * dt * drift;
    n.x += Math.sin(t * 0.9 + n.phase) * (0.012 + energy * 0.02);
    n.y += Math.cos(t * 0.8 + n.phase) * (0.012 + energy * 0.02);

    if (n.x < -20) n.x = window.innerWidth + 20;
    if (n.x > window.innerWidth + 20) n.x = -20;
    if (n.y < -20) n.y = window.innerHeight + 20;
    if (n.y > window.innerHeight + 20) n.y = -20;
  }

  const baseLinkAlpha = 0.06 + energy * 0.14;
  for (let i = 0; i < NODES.length; i++) {
    for (let j = i + 1; j < NODES.length; j++) {
      const a = NODES[i], b = NODES[j];
      const dx = a.x - b.x, dy = a.y - b.y;
      const d2 = dx * dx + dy * dy;
      if (d2 > LINK_DIST * LINK_DIST) continue;
      const d = Math.sqrt(d2);
      const w = 1 - d / LINK_DIST;

      ctx.strokeStyle = `rgba(103,232,249,${baseLinkAlpha * w})`;
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(a.x, a.y);
      ctx.lineTo(b.x, b.y);
      ctx.stroke();
    }
  }

  const nodeAlpha = 0.18 + energy * 0.32;
  for (const n of NODES) {
    const pulse = (Math.sin(t * 1.6 + n.phase) + 1) / 2;
    const r = n.r + pulse * (0.35 + energy * 0.8);

    ctx.fillStyle = `rgba(34,211,238,${nodeAlpha})`;
    ctx.beginPath();
    ctx.arc(n.x, n.y, r, 0, Math.PI * 2);
    ctx.fill();
  }

  if (Math.random() < PULSE_SPAWN_BASE + energy * 0.06) {
    const pick = NODES[(Math.random() * NODES.length) | 0];
    spawnPulse(pick.x, pick.y, energy);
  }

  for (let i = PULSES.length - 1; i >= 0; i--) {
    const p = PULSES[i];
    p.r += p.speed;
    p.life--;
    const a = Math.max(0, (p.life / 140)) * p.alpha * (0.55 + energy * 0.7);
    ctx.strokeStyle = `rgba(103,232,249,${a})`;
    ctx.beginPath();
    ctx.arc(p.x, p.y, p.r, 0, Math.PI * 2);
    ctx.stroke();
    if (p.life <= 0) PULSES.splice(i, 1);
  }

  drawVignette();
  requestAnimationFrame(renderNeural);
}
requestAnimationFrame(renderNeural);

/* ============================================================
   BOOT FX (裂隙 + 能量脉冲)
   ============================================================ */

const bootfx = document.getElementById('bootfx');
const bootfxCtx = bootfx ? bootfx.getContext('2d') : null;

let bootRunning = !!bootfxCtx;
const bootStart = performance.now();

const fractures = [];
const sweeps = [];

/* 🔧 FIX: 能量脉冲（替代白闪） */
const bootPulses = [];
function spawnBootPulse(x, y, strength = 1) {
  bootPulses.push({
    x, y,
    r: 0,
    speed: 16 * strength,
    life: 36,
    alpha: 0.45 * strength
  });
}

function spawnFracture() {
  const vertical = Math.random() > 0.5;
  fractures.push({
    x: vertical ? rand(0, window.innerWidth) : 0,
    y: vertical ? 0 : rand(0, window.innerHeight),
    vx: vertical ? rand(-1, 1) : rand(2, 5),
    vy: vertical ? rand(2, 5) : rand(-1, 1),
    life: rand(20, 60),
    w: rand(1, 3)
  });
}

function spawnSweep() {
  sweeps.push({
    angle: rand(0, Math.PI),
    offset: rand(-window.innerWidth, window.innerWidth),
    speed: rand(12, 28),
    width: rand(40, 120),
    life: rand(20, 40)
  });
}

function scanlines(alpha) {
  bootfxCtx.save();
  bootfxCtx.globalAlpha = alpha;
  bootfxCtx.fillStyle = '#fff';
  for (let y = 0; y < window.innerHeight; y += 3) {
    bootfxCtx.fillRect(0, y, window.innerWidth, 1);
  }
  bootfxCtx.restore();
}

function noise(alpha) {
  bootfxCtx.save();
  bootfxCtx.globalAlpha = alpha;
  bootfxCtx.fillStyle = '#000';
  for (let i = 0; i < 8000; i++) {
    bootfxCtx.fillRect(Math.random() * window.innerWidth, Math.random() * window.innerHeight, 1, 1);
  }
  bootfxCtx.restore();
}

function renderBoot(now) {
  if (!bootRunning || !bootfxCtx) return;

  const t = (now - bootStart) / 1000;
  bootfxCtx.fillStyle = 'rgba(0,0,0,0.25)';
  bootfxCtx.fillRect(0, 0, window.innerWidth, window.innerHeight);

  if (t < 0.4) noise(0.12);
  if (t > 0.4 && t < 1.6 && Math.random() < 0.4) spawnFracture();

  fractures.forEach(f => {
    bootfxCtx.strokeStyle = 'rgba(180,220,255,0.35)';
    bootfxCtx.lineWidth = f.w;
    bootfxCtx.beginPath();
    bootfxCtx.moveTo(f.x, f.y);
    bootfxCtx.lineTo(f.x + f.vx * 80, f.y + f.vy * 80);
    bootfxCtx.stroke();
    f.x += f.vx * 6;
    f.y += f.vy * 6;
    f.life--;
  });

  for (let i = fractures.length - 1; i >= 0; i--) {
    if (fractures[i].life <= 0) fractures.splice(i, 1);
  }

  if (t > 1.2 && t < 2.6 && Math.random() < 0.25) spawnSweep();

  sweeps.forEach(s => {
    bootfxCtx.save();
    bootfxCtx.translate(window.innerWidth / 2, window.innerHeight / 2);
    bootfxCtx.rotate(s.angle);
    bootfxCtx.fillStyle = 'rgba(220,245,255,0.18)';
    bootfxCtx.fillRect(-window.innerWidth, s.offset, window.innerWidth * 2, s.width);
    bootfxCtx.restore();
    s.offset += s.speed;
    s.life--;
  });

  for (let i = sweeps.length - 1; i >= 0; i--) {
    if (sweeps[i].life <= 0) sweeps.splice(i, 1);
  }

  if (t > 2.4 && t < 3.1) scanlines(0.18);

  /* 🔧 FIX: 用能量脉冲替代白闪 */
  if (t > 3.12 && t < 3.18 && bootPulses.length === 0) {
    spawnBootPulse(window.innerWidth * 0.5, window.innerHeight * 0.45, 1.2);
    spawnBootPulse(window.innerWidth * 0.46, window.innerHeight * 0.48, 0.6);
  }

  for (let i = bootPulses.length - 1; i >= 0; i--) {
    const p = bootPulses[i];
    p.r += p.speed;
    p.life--;
    const a = p.alpha * (p.life / 36);
    bootfxCtx.strokeStyle = `rgba(180,235,255,${a})`;
    bootfxCtx.beginPath();
    bootfxCtx.arc(p.x, p.y, p.r, 0, Math.PI * 2);
    bootfxCtx.stroke();
    if (p.life <= 0) bootPulses.splice(i, 1);
  }

  requestAnimationFrame(renderBoot);
}
if (bootRunning) requestAnimationFrame(renderBoot);

/* ============================================================
   BOOT LOG（原样保留）
   ============================================================ */

const bootLines = [
  'Reality fabric unstable',
  'Spatial breach detected',
  'Neural authority overriding',
  'System perception reassigned',
  'AI CORE ONLINE'
];

const bootLog = document.getElementById('boot-log');
let bootIdx = 0;

const bootTimer = setInterval(() => {
  if (bootIdx < bootLines.length) {
    const line = document.createElement('div');
    line.textContent = bootLines[bootIdx++];
    bootLog.appendChild(line);
    bumpActivity(0.08);
  } else {
    clearInterval(bootTimer);
  }
}, 420);

/* ============================================================
   EXIT BOOT（🔧 FIX: 脉冲无缝接管）
   ============================================================ */

setTimeout(() => {
  // 把 boot 能量注入 Neural Field
  for (const p of bootPulses) {
    PULSES.push({
      x: p.x,
      y: p.y,
      r: p.r,
      speed: p.speed * 0.9,
      life: Math.floor(p.life * 2),
      alpha: p.alpha * 0.85
    });
  }

  bootRunning = false;
  const bootEl = document.getElementById('boot');
  bootEl && bootEl.remove();

  document.getElementById('app').classList.remove('hidden');

  bumpActivity(0.35);
  spawnPulse(window.innerWidth * 0.5, window.innerHeight * 0.45, 0.9);
}, 3300);

/* ============================================================
   CORE HEALTH MANAGER（你原来的逻辑：保留）
   ============================================================ */

const coreLed = document.getElementById('core-led');
const coreStatus = document.getElementById('core-status');

let coreOnline = false;
const healthHistory = [];
const HEALTH_HISTORY_SIZE = 5;
const HEALTH_URL = '/health';

async function checkHealth() {
  let ok = false;
  try {
    const resp = await fetch(HEALTH_URL, { cache: 'no-store' });
    const text = await resp.text();
    ok = text.trim().toUpperCase() === 'OK';
  } catch {
    ok = false;
  }
  healthHistory.push(ok);
  if (healthHistory.length > HEALTH_HISTORY_SIZE) healthHistory.shift();
  applyHealthState();
}

function applyHealthState() {
  const hasOk = healthHistory.includes(true);
  const hasFail = healthHistory.includes(false);

  if (hasOk && hasFail) setCoreFlapping();
  else if (hasOk) setCoreStatus(true);
  else setCoreStatus(false);
}

function setCoreFlapping() {
  coreOnline = true;
  coreLed.classList.add('on', 'blink');
  coreStatus.textContent = 'UNSTABLE';
  coreStatus.classList.add('online');
  coreStatus.classList.remove('offline');
  enableInput(true);

  bumpActivity(0.18);
  spawnPulse(window.innerWidth * rand(0.35, 0.65), window.innerHeight * rand(0.35, 0.65), 0.6);
}

function setCoreStatus(online) {
  coreOnline = online;
  coreLed.classList.remove('blink');

  if (online) {
    coreLed.classList.add('on');
    coreStatus.textContent = 'ONLINE';
    coreStatus.classList.add('online');
    coreStatus.classList.remove('offline');
    enableInput(true);

    bumpActivity(0.22);
    spawnPulse(window.innerWidth * 0.52, window.innerHeight * 0.40, 0.7);
  } else {
    coreLed.classList.remove('on');
    coreStatus.textContent = 'OFFLINE';
    coreStatus.classList.add('offline');
    coreStatus.classList.remove('online');
    enableInput(false);
    forceIdle();

    activity *= 0.55;
  }
}

setInterval(checkHealth, 3000);
checkHealth();

/* ============================================================
   TOASTS (silent UX feedback for notice-only events)
   ============================================================ */

const toastRoot = (() => {
  const el = document.createElement('div');
  el.id = 'toast-root';
  document.body.appendChild(el);
  return el;
})();

function showToast(message, variant = 'ok', ttlMs = 1800) {
  if (!message) return;
  const t = document.createElement('div');
  t.className = `toast ${variant}`;
  t.textContent = message;
  toastRoot.appendChild(t);

  // allow CSS transition
  requestAnimationFrame(() => t.classList.add('show'));

  const kill = () => {
    t.classList.remove('show');
    setTimeout(() => t.remove(), 240);
  };
  setTimeout(kill, ttlMs);
}

/* ============================================================
   ASSISTANT OUTPUT SANITIZER (UI-only)
   - Backend sanitizes the stored assistant message, but streaming deltas
     may render noisy prefixes/disclaimers in the chat bubble.
   - We clean the final rendered text after stream completes.
   ============================================================ */

function sanitizeAssistantFinalText(s) {
  let out = String(s || '');

  // Strip misleading "I remembered" claims (model sometimes hallucinates this).
  // Memory/facts are handled silently; don't surface these prefixes.
  out = out.replace(/^\s*(我)?(已记住|已记录|我会记住|我已记录|我已经记住)[：:]\s*/u, '');

  // Remove parenthetical boilerplate about identity contract / rules.
  out = out.replace(/[（(][^（）()]*?(身份契约|指代规则|准确记录用户偏好)[^（）()]*?[）)]/gu, '');

  // Collapse extra whitespace/newlines introduced by removals.
  out = out.replace(/\n{3,}/g, '\n\n');
  out = out.replace(/[ \t]{2,}/g, ' ');

  return out.trim();
}

/* ============================================================
   FACTS CENTER (pending groups / conflicts / versions)
   ============================================================ */

const factsBtn = document.getElementById('facts-btn');
const factsLed = document.getElementById('facts-led');
const factsCount = document.getElementById('facts-count');
const factsOverlay = document.getElementById('facts-overlay');
const factsClose = document.getElementById('facts-close');
const factsTabs = Array.from(document.querySelectorAll('.facts-tab'));
const factsBody = document.querySelector('.facts-body');

const panePending = document.getElementById('facts-pane-pending');
const paneConflicts = document.getElementById('facts-pane-conflicts');
const paneActive = document.getElementById('facts-pane-active');
const paneHistory = document.getElementById('facts-pane-history');

const debugBtn = document.getElementById('debug-btn');
const debugLed = document.getElementById('debug-led');
const debugOverlay = document.getElementById('debug-overlay');
const debugClose = document.getElementById('debug-close');
const debugBody = document.getElementById('debug-body');

let lastUserInput = '';

let debugHadError = false;

function refreshDebugLed() {
  if (!debugLed) return;
  debugLed.classList.remove('on', 'blink');
  if (lastUserInput) {
    debugLed.classList.add('on');
    if (debugHadError) debugLed.classList.add('blink');
  }
}

function setFactsState(pending, conflicts) {
  const p = Number(pending || 0);
  const c = Number(conflicts || 0);
  const total = p + c;
  factsCount.textContent = String(total);

  if (total > 0) {
    factsLed.classList.add('on');
  } else {
    factsLed.classList.remove('on');
  }

  // 冲突存在时，LED 闪烁提示
  if (c > 0) {
    factsLed.classList.add('blink');
  } else {
    factsLed.classList.remove('blink');
  }
}

async function fetchFactCounts() {
  try {
    const resp = await fetch('/api/facts/status/counts', { cache: 'no-store' });
    if (!resp.ok) {
      // avoid stale LED state when backend glitches
      setFactsState(0, 0);
      return null;
    }
    const data = await resp.json();
    const p = Number(data.pending || 0);
    const c = Number(data.conflicts || 0);
    setFactsState(p, c);
    return { pending: p, conflicts: c };
  } catch {
    // avoid stale LED state when network fails
    setFactsState(0, 0);
    return null;
  }
}

function escapeHtml(s) {
  return (s || '').replace(/[&<>"']/g, (c) => ({
    '&':'&amp;',
    '<':'&lt;',
    '>':'&gt;',
    '"':'&quot;',
    "'":'&#39;'
  }[c]));
}

function makeFactRow(textHtml, metaText, buttons) {
  const row = document.createElement('div');
  row.className = 'fact-row';

  const main = document.createElement('div');
  main.className = 'fact-text';
  main.innerHTML = `<div>${textHtml}</div>${metaText ? `<div class="fact-meta">${escapeHtml(metaText)}</div>` : ''}`;

  const actions = document.createElement('div');
  actions.className = 'fact-actions';
  for (const b of buttons) actions.appendChild(b);

  row.appendChild(main);
  row.appendChild(actions);
  return row;
}

function setActiveTab(tabName) {
  for (const t of factsTabs) {
    const on = (t.getAttribute('data-tab') === tabName);
    t.classList.toggle('active', on);
  }
  panePending.classList.toggle('hidden', tabName !== 'pending');
  paneConflicts.classList.toggle('hidden', tabName !== 'conflicts');
  paneActive.classList.toggle('hidden', tabName !== 'active');
  paneHistory.classList.toggle('hidden', tabName !== 'history');
}

async function loadPendingGroups() {
  panePending.innerHTML = '<div class="fact-meta">Loading…</div>';
  try {
    const resp = await fetch('/api/facts/pending/groups', { cache: 'no-store' });
    if (!resp.ok) {
      panePending.innerHTML = '<div class="fact-meta">Failed to load.</div>';
      return;
    }
    const data = await resp.json();
    const groups = data.groups || [];
    if (groups.length === 0) {
      panePending.innerHTML = '<div class="fact-meta">No pending facts.</div>';
      return;
    }

    panePending.innerHTML = '';
    for (const g of groups) {
      const wrap = document.createElement('div');
      wrap.className = 'fact-group';

      const head = document.createElement('div');
      head.className = 'fact-group-head';
      const title = document.createElement('div');
      title.className = 'fact-group-title';
      title.textContent = g.rep?.fact || '(empty)';
      const badge = document.createElement('div');
      badge.className = 'fact-group-badge';
      badge.textContent = `×${g.size || (g.items ? g.items.length : 0)}`;
      head.appendChild(title);
      head.appendChild(badge);

      const body = document.createElement('div');
      body.className = 'fact-group-body';

      const ids = (g.items || []).map(x => x.id);
      const btnRememberAll = document.createElement('button');
      btnRememberAll.className = 'fact-btn primary';
      btnRememberAll.textContent = 'REMEMBER GROUP';
      btnRememberAll.onclick = async (e) => {
        e.stopPropagation();
        btnRememberAll.disabled = true;
        try {
          const res = await fetch('/api/facts/remember_batch', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ ids: ids })
          });
          if (!res.ok) {
            const t = await res.text();
            showToast(t || 'remember batch failed', 'err', 2600);
          }
        } finally {
          await refreshFactsUI();
        }
      };

      const btnRejectAll = document.createElement('button');
      btnRejectAll.className = 'fact-btn danger';
      btnRejectAll.textContent = 'REJECT GROUP';
      btnRejectAll.onclick = async (e) => {
        e.stopPropagation();
        btnRejectAll.disabled = true;
        try {
          const res = await fetch('/api/facts/reject_batch', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ ids: ids })
          });
          if (!res.ok) {
            const t = await res.text();
            showToast(t || 'reject batch failed', 'err', 2600);
          }
        } finally {
          await refreshFactsUI();
        }
      };

      // group actions row
      const groupActions = document.createElement('div');
      groupActions.style.display = 'flex';
      groupActions.style.gap = '10px';
      groupActions.style.marginBottom = '10px';
      groupActions.appendChild(btnRememberAll);
      groupActions.appendChild(btnRejectAll);
      body.appendChild(groupActions);

      for (const it of (g.items || [])) {
        const confVal = (typeof it.confidence === "number") ? it.confidence : Number(it.confidence || 0);
        const confText = Number.isFinite(confVal) ? confVal.toFixed(2) : String(it.confidence || "");
        const meta = `conf=${confText} · ${it.source_key || ''}`;

        const btnRemember = document.createElement('button');
        btnRemember.className = 'fact-btn primary';
        btnRemember.textContent = 'REMEMBER';
        btnRemember.onclick = async () => {
          btnRemember.disabled = true;
          try {
            await fetch('/api/facts/remember', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ id: it.id })
            });
          } finally {
            await refreshFactsUI();
          }
        };

        const btnReject = document.createElement('button');
        btnReject.className = 'fact-btn danger';
        btnReject.textContent = 'REJECT';
        btnReject.onclick = async () => {
          btnReject.disabled = true;
          try {
            await fetch('/api/facts/reject', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ id: it.id })
            });
          } finally {
            await refreshFactsUI();
          }
        };

        const row = makeFactRow(escapeHtml(it.fact), meta, [btnRemember, btnReject]);
        body.appendChild(row);
      }

      head.onclick = () => {
        body.classList.toggle('hidden');
      };

      wrap.appendChild(head);
      wrap.appendChild(body);
      panePending.appendChild(wrap);
    }
  } catch {
    panePending.innerHTML = '<div class="fact-meta">Failed to load.</div>';
  }
}

async function loadConflicts() {
  paneConflicts.innerHTML = '<div class="fact-meta">Loading…</div>';
  try {
    const resp = await fetch('/api/facts/conflicts', { cache: 'no-store' });
    if (!resp.ok) {
      paneConflicts.innerHTML = '<div class="fact-meta">Failed to load.</div>';
      return;
    }
    const data = await resp.json();
    const items = data.items || [];
    if (items.length === 0) {
      paneConflicts.innerHTML = '<div class="fact-meta">No conflicts.</div>';
      return;
    }
    paneConflicts.innerHTML = '';
    for (const c of items) {
      const text = `<div><b>EXISTING</b>\n${escapeHtml(c.existing_fact || '')}</div><div style="margin-top:10px"><b>PROPOSED</b>\n${escapeHtml(c.proposed_fact || '')}</div>`;

      const btnKeep = document.createElement('button');
      btnKeep.className = 'fact-btn';
      btnKeep.textContent = 'KEEP';
      btnKeep.onclick = async () => {
        btnKeep.disabled = true;
        try {
          await fetch('/api/facts/conflicts/keep', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ id: c.id })
          });
        } finally {
          await refreshFactsUI();
        }
      };

      const btnReplace = document.createElement('button');
      btnReplace.className = 'fact-btn primary';
      btnReplace.textContent = 'REPLACE';
      btnReplace.onclick = async () => {
        btnReplace.disabled = true;
        try {
          await fetch('/api/facts/conflicts/replace', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ id: c.id })
          });
        } finally {
          await refreshFactsUI();
        }
      };

      const btnEdit = document.createElement('button');
      btnEdit.className = 'fact-btn';
      btnEdit.textContent = 'EDIT';
      btnEdit.onclick = async () => {
        const v = prompt('Edit replacement fact:', c.proposed_fact || '');
        if (v == null) return;
        btnEdit.disabled = true;
        try {
          await fetch('/api/facts/conflicts/replace', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ id: c.id, replacement: v })
          });
        } finally {
          await refreshFactsUI();
        }
      };

      const row = makeFactRow(text.replace(/\n/g, '<br/>'), `key=${c.fact_key || ''}`, [btnKeep, btnReplace, btnEdit]);
      paneConflicts.appendChild(row);
    }
  } catch {
    paneConflicts.innerHTML = '<div class="fact-meta">Failed to load.</div>';
  }
}

async function loadActiveFacts() {
  paneActive.innerHTML = '<div class="fact-meta">Loading…</div>';
  try {
    const resp = await fetch('/api/facts/active', { cache: 'no-store' });
    if (!resp.ok) {
      paneActive.innerHTML = '<div class="fact-meta">Failed to load.</div>';
      return;
    }
    const data = await resp.json();
    const items = data.items || [];
    if (items.length === 0) {
      paneActive.innerHTML = '<div class="fact-meta">No active facts.</div>';
      return;
    }
    paneActive.innerHTML = '';
    for (const it of items) {
      const meta = `${it.fact_key || ''} · updated ${it.updated_at || ''}`;
      const row = makeFactRow(escapeHtml(it.fact || ''), meta, []);
      paneActive.appendChild(row);
    }
  } catch {
    paneActive.innerHTML = '<div class="fact-meta">Failed to load.</div>';
  }
}

async function loadHistory() {
  paneHistory.innerHTML = '<div class="fact-meta">Loading…</div>';
  try {
    const resp = await fetch('/api/facts/history?limit=200', { cache: 'no-store' });
    if (!resp.ok) {
      paneHistory.innerHTML = '<div class="fact-meta">Failed to load.</div>';
      return;
    }
    const data = await resp.json();
    const items = data.items || [];
    if (items.length === 0) {
      paneHistory.innerHTML = '<div class="fact-meta">No history.</div>';
      return;
    }
    paneHistory.innerHTML = '';
    for (const it of items) {
      const meta = `${it.status || ''} · v${it.version || 0} · ${it.fact_key || ''} · ${it.created_at || ''}`;
      const row = makeFactRow(escapeHtml(it.fact || ''), meta, []);
      paneHistory.appendChild(row);
    }
  } catch {
    paneHistory.innerHTML = '<div class="fact-meta">Failed to load.</div>';
  }
}

async function loadFactsTab(tabName) {
  setActiveTab(tabName);
  if (tabName === 'pending') return loadPendingGroups();
  if (tabName === 'conflicts') return loadConflicts();
  if (tabName === 'active') return loadActiveFacts();
  if (tabName === 'history') return loadHistory();
}

async function refreshFactsUI() {
  const counts = await fetchFactCounts();
  let active = (factsTabs.find(t => t.classList.contains('active')) || factsTabs[0]).getAttribute('data-tab');

  // 如果当前停在 PENDING，但 pending 已清空且存在冲突，自动切到 CONFLICTS
  if (counts && active === 'pending' && counts.pending === 0 && counts.conflicts > 0) {
    active = 'conflicts';
  }

  await loadFactsTab(active);
}

async function openFacts() {
  factsOverlay.classList.remove('hidden');
  const counts = await fetchFactCounts();
  let active = (factsTabs.find(t => t.classList.contains('active')) || factsTabs[0]).getAttribute('data-tab');

  // 没有 pending 但有 conflicts 时，直接展示 conflicts，避免 LED 亮但页面空的困惑
  if (counts && counts.pending === 0 && counts.conflicts > 0) {
    active = 'conflicts';
  }

  await loadFactsTab(active);
}

function closeFacts() {
  factsOverlay.classList.add('hidden');
}

factsBtn?.addEventListener('click', openFacts);
factsClose?.addEventListener('click', closeFacts);
factsOverlay?.addEventListener('click', (e) => {
  if (e.target === factsOverlay) closeFacts();
});

factsTabs.forEach(t => {
  t.addEventListener('click', () => loadFactsTab(t.getAttribute('data-tab')));
});

setInterval(fetchFactCounts, 6000);
fetchFactCounts();

function openDebug() {
  debugOverlay?.classList.remove('hidden');
  loadDebugContext();
}

function closeDebug() {
  debugOverlay?.classList.add('hidden');
}

async function loadDebugContext() {
  if (!debugBody) return;
  if (!lastUserInput) {
    debugHadError = false;
    refreshDebugLed();
    debugBody.innerHTML = '<div class="debug-card"><h3>NO INPUT</h3><div class="debug-pre">Send one message first, then open DEBUG.</div></div>';
    return;
  }
  debugBody.innerHTML = '<div class="debug-card"><h3>LOADING</h3><div class="debug-pre">…</div></div>';
  debugHadError = false;
  refreshDebugLed();
  try {
    const resp = await fetch('/api/debug/context', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ question: lastUserInput })
    });
    if (!resp.ok) {
      debugHadError = true;
      refreshDebugLed();
      debugBody.innerHTML = '<div class="debug-card"><h3>ERROR</h3><div class="debug-pre">Failed to fetch debug context.</div></div>';
      return;
    }
    const a = await resp.json();
    const meta = `date=${a.date || ''}\nquestion=${a.question || ''}\nblocks=${(a.blocks || []).length}\nsearch_hits=${(a.search_hits || []).length}`;

    const metaCard = document.createElement('div');
    metaCard.className = 'debug-card';
    metaCard.innerHTML = `<h3>META</h3><div class="debug-pre">${escapeHtml(meta)}</div>`;
    debugBody.innerHTML = '';
    debugBody.appendChild(metaCard);

    const stepsCard = document.createElement('div');
    stepsCard.className = 'debug-card';
    stepsCard.innerHTML = `<h3>STEPS</h3><div class="debug-pre">${escapeHtml((a.steps || []).join('\n'))}</div>`;
    debugBody.appendChild(stepsCard);

    const blocksCard = document.createElement('div');
    blocksCard.className = 'debug-card';
    const views = a.blocks_view || [];
    const lines = views.map((b, i) => `#${i+1} [${b.source}] role=${b.role} len=${b.len}\n${b.preview}`);
    blocksCard.innerHTML = `<h3>INJECTED BLOCKS</h3><div class="debug-pre">${escapeHtml(lines.join('\n\n'))}</div>`;
    debugBody.appendChild(blocksCard);

    if (a.search_hits && a.search_hits.length) {
      const hitsCard = document.createElement('div');
      hitsCard.className = 'debug-card';
      const hlines = a.search_hits.map((h, i) => `#${i+1} score=${h.score?.toFixed ? h.score.toFixed(3) : h.score}\n${h.text}`);
      hitsCard.innerHTML = `<h3>SEARCH HITS</h3><div class="debug-pre">${escapeHtml(hlines.join('\n\n'))}</div>`;
      debugBody.appendChild(hitsCard);
    }
  } catch {
    debugHadError = true;
    refreshDebugLed();
    debugBody.innerHTML = '<div class="debug-card"><h3>ERROR</h3><div class="debug-pre">Failed to fetch debug context.</div></div>';
  }
}

debugBtn?.addEventListener('click', openDebug);
debugClose?.addEventListener('click', closeDebug);
debugOverlay?.addEventListener('click', (e) => {
  if (e.target === debugOverlay) closeDebug();
});

/* ============================================================
   AUTO SCROLL（你原来的逻辑：保留）
   ============================================================ */

const elLog = document.getElementById('log');
const elInput = document.getElementById('input');
const elSend = document.getElementById('send');
const elStatus = document.getElementById('status');
const elComposer = document.getElementById('composer');

const AUTO_SCROLL_THRESHOLD = 80;

function isNearBottom(el) {
  return el.scrollHeight - el.scrollTop - el.clientHeight < AUTO_SCROLL_THRESHOLD;
}

function scrollToBottom(el) {
  el.scrollTop = el.scrollHeight;
}

function maybeAutoScroll(el) {
  if (isNearBottom(el)) scrollToBottom(el);
}

/* ============================================================
   THINKING（你原来的逻辑：保留）
   - ✅ CSS 能量条已删除，这里只保留字符波形
   ============================================================ */

let thinkingTimer = null;
let thinkingStep = 0;
const waveFrames = [
  '◦   ◦   ◦   ◦',
  '◦   •   •   ◦',
  '•   •   •   •',
  '◦   •   •   ◦'
];

/* ============================================================
   NEW / JUMP / TRIM / GLOW（你原来的逻辑：保留）
   ============================================================ */

let userAtBottom = true;

const MAX_MESSAGES = 220;
const TRIM_BATCH = 30;

/* ===== 悬浮 NEW / 跳到底部按钮 ===== */

const jumpBtn = document.createElement('button');
jumpBtn.type = 'button';
jumpBtn.textContent = '⬇ NEW';
jumpBtn.style.cssText = `
  position: fixed;
  right: 26px;
  bottom: 118px;
  padding: 9px 14px;
  border-radius: 999px;
  border: 1px solid rgba(148,163,184,.28);
  background: rgba(2,6,23,.82);
  color: #67e8f9;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  letter-spacing: .6px;
  cursor: pointer;
  display: none;
  z-index: 9999;
  backdrop-filter: blur(10px);
  transition: opacity .15s ease, transform .15s ease;
  opacity: 0;
`;
document.body.appendChild(jumpBtn);

function showJumpBtn() {
  if (jumpBtn.style.display === 'block') return;
  jumpBtn.style.display = 'block';
  requestAnimationFrame(() => {
    jumpBtn.style.opacity = '1';
    jumpBtn.style.transform = 'translateY(0)';
  });
}

function hideJumpBtn() {
  if (jumpBtn.style.display === 'none') return;
  jumpBtn.style.opacity = '0';
  jumpBtn.style.transform = 'translateY(6px)';
  setTimeout(() => {
    if (userAtBottom) jumpBtn.style.display = 'none';
  }, 160);
}

jumpBtn.onclick = () => {
  scrollToBottom(elLog);
  userAtBottom = true;
  hideJumpBtn();
};

elLog.addEventListener('scroll', () => {
  userAtBottom = isNearBottom(elLog);
  if (userAtBottom) hideJumpBtn();
});

/* ===== DOM 裁剪 ===== */

function trimMessagesIfNeeded() {
  const count = elLog.children.length;
  if (count <= MAX_MESSAGES) return;

  const removeCount = Math.min(TRIM_BATCH, count - MAX_MESSAGES);
  const before = elLog.scrollHeight;

  for (let i = 0; i < removeCount; i++) {
    elLog.firstChild && elLog.removeChild(elLog.firstChild);
  }

  const after = elLog.scrollHeight;
  elLog.scrollTop -= (before - after);
}

/* ===== GLOW（你原来的逻辑：保留） ===== */

function glowOn(el) {
  el.style.setProperty('text-shadow', '0 0 14px rgba(103,232,249,.38)', 'important');
  el.style.setProperty(
      'box-shadow',
      '0 0 0 1px rgba(103,232,249,.14), 0 0 28px rgba(103,232,249,.16)',
      'important'
  );
  el.style.setProperty('transition', 'box-shadow .9s ease, filter .9s ease', 'important');

  if (el.__breathTimer) clearInterval(el.__breathTimer);
  let on = false;
  el.__breathTimer = setInterval(() => {
    on = !on;
    el.style.setProperty('filter', on ? 'brightness(1.08)' : 'brightness(1)', 'important');
    el.style.setProperty(
        'box-shadow',
        on
            ? '0 0 0 1px rgba(103,232,249,.18), 0 0 38px rgba(103,232,249,.22)'
            : '0 0 0 1px rgba(103,232,249,.12), 0 0 22px rgba(103,232,249,.14)',
        'important'
    );
  }, 900);
}

function glowOff(el) {
  el.style.setProperty('text-shadow', '', 'important');
  el.style.setProperty('box-shadow', '', 'important');
  el.style.setProperty('filter', '', 'important');
  if (el.__breathTimer) {
    clearInterval(el.__breathTimer);
    el.__breathTimer = null;
  }
}

/* ===== Typewriter（你原来的逻辑：保留） ===== */

function createTypewriter(targetEl) {
  let pending = '';
  let raf = 0;

  function flush() {
    raf = 0;
    if (!pending) return;
    targetEl.textContent += pending;
    pending = '';
    userAtBottom ? scrollToBottom(elLog) : showJumpBtn();
  }

  return {
    push(text) {
      pending += text;
      if (!raf) raf = requestAnimationFrame(flush);
    },
    finish() {
      if (raf) cancelAnimationFrame(raf);
      if (pending) targetEl.textContent += pending;
      pending = '';
    }
  };
}

/* ============================================================
   INPUT = 能量注入槽（新增保留）
   ============================================================ */

let injectTimer = null;
function setInjecting(on) {
  if (!elComposer) return;
  if (on) elComposer.classList.add('injecting');
  else elComposer.classList.remove('injecting');
}

function fireComposer() {
  if (!elComposer) return;
  elComposer.classList.add('fire');
  setTimeout(() => elComposer.classList.remove('fire'), 260);
}

function touchInject() {
  setInjecting(true);
  bumpActivity(0.06);
  if (Math.random() < 0.12) spawnPulse(window.innerWidth * 0.5, window.innerHeight * 0.78, 0.35);

  injectTimer && clearTimeout(injectTimer);
  injectTimer = setTimeout(() => setInjecting(false), 520);
}

elInput.addEventListener('input', touchInject);
elInput.addEventListener('focus', () => setInjecting(true));
elInput.addEventListener('blur', () => setInjecting(false));

/* ============================================================
   状态控制（你原来的逻辑：保留）
   ============================================================ */

function setBusy(b) {
  if (!coreOnline) return;
  if (b) {
    if (thinkingTimer) return;
    elStatus.classList.add('thinking');
    thinkingTimer = setInterval(() => {
      elStatus.textContent = 'THINKING  ' + waveFrames[thinkingStep++ % waveFrames.length];
    }, 180);

    bumpActivity(0.15);
  } else {
    forceIdle();
  }
}

function forceIdle() {
  thinkingTimer && clearInterval(thinkingTimer);
  thinkingTimer = null;
  elStatus.classList.remove('thinking');
  elStatus.textContent = '(•‿•)';
}

function enableInput(enable) {
  elInput.disabled = !enable;
  elSend.disabled = !enable;
}

/* ============================================================
   SSE 聊天（你原来的逻辑：保留 + 背景响应）
   ============================================================ */
async function sendStream(input) {
  if (!coreOnline) return;

  // for /api/debug/context
  lastUserInput = String(input || '').trim();
  debugHadError = false;
  refreshDebugLed();

  setBusy(true);

  fireComposer();
  bumpActivity(0.22);
  spawnPulse(window.innerWidth * 0.5, window.innerHeight * 0.80, 0.9);

  const userMsg = document.createElement('div');
  userMsg.className = 'msg user';
  userMsg.textContent = input;
  elLog.appendChild(userMsg);
  scrollToBottom(elLog);

  const aiMsg = document.createElement('div');
  aiMsg.className = 'msg ai';

  /* ===== COPY BUTTON ===== */
  const copyBtn = document.createElement('div');
  copyBtn.className = 'copy-btn';
  copyBtn.textContent = 'COPY';

  /* ===== AI CONTENT WRAPPER ===== */
  const aiContent = document.createElement('div');
  aiContent.className = 'ai-content';

  copyBtn.onclick = async () => {
    try {
      await navigator.clipboard.writeText(aiContent.textContent);
      copyBtn.textContent = 'COPIED ✓';
      copyBtn.classList.add('copied');

      setTimeout(() => {
        copyBtn.textContent = 'COPY';
        copyBtn.classList.remove('copied');
      }, 1200);
    } catch {
      copyBtn.textContent = 'FAILED';
      setTimeout(() => (copyBtn.textContent = 'COPY'), 1200);
    }
  };

  aiMsg.appendChild(copyBtn);
  aiMsg.appendChild(aiContent);

  elLog.appendChild(aiMsg);
  scrollToBottom(elLog);

  const typer = createTypewriter(aiContent);

  glowOn(aiMsg);
  trimMessagesIfNeeded();

  // ✅ 流式期间开扫光
  aiMsg.classList.add('streaming');

  try {
    const resp = await fetch('/api/chat/stream', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ input })
    });

    if (!resp.ok || !resp.body) {
      typer.push(`[error] HTTP ${resp.status}`);
      debugHadError = true;
      refreshDebugLed();
      return;
    }

    const reader = resp.body.getReader();
    const decoder = new TextDecoder('utf-8');
    let buf = '';
    let gotAny = false;
    let renderedAnyText = false;
    let hadError = false;

    // Streaming prefix stripper: prevents brief flashes of "已记住：" etc.
    let memPrefixBuf = '';
    let memPrefixDone = false;
    const memPrefixRe = /^\s*(我)?(已记住|已记录|我会记住|我已记录|我已经记住)[：:]\s*/u;
    const pushDeltaClean = (delta) => {
      const d = String(delta || '');
      if (!d) return;
      if (memPrefixDone) {
        typer.push(d);
        return;
      }
      memPrefixBuf += d;

      if (memPrefixRe.test(memPrefixBuf) || memPrefixBuf.length >= 24) {
        memPrefixBuf = memPrefixBuf.replace(memPrefixRe, '');
        memPrefixDone = true;
        if (memPrefixBuf) typer.push(memPrefixBuf);
        memPrefixBuf = '';
      }
    };

    while (true) {
      const { value, done } = await reader.read();
      if (done) break;

      buf += decoder.decode(value, { stream: true });

      let idx;
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const frame = buf.slice(0, idx);
        buf = buf.slice(idx + 2);

        if (!frame.startsWith('data: ')) continue;

        const obj = JSON.parse(frame.slice(6));

        if (obj.error) {
          gotAny = true;
          hadError = true;
          debugHadError = true;
          refreshDebugLed();
          typer.push(`[error] ${obj.error}`);
          continue;
        }

        // Meta/notice-only events (e.g. facts remember/forget) should be silent in chat.
        if (obj.notice) {
          gotAny = true;
          if (obj.notice === 'facts') {
            // Silent UX: only refresh FACTS LED/counts, no chat bubble/toast.
            await fetchFactCounts();
          }
          continue;
        }
        if (obj.delta) {
          gotAny = true;
          renderedAnyText = true;
          pushDeltaClean(obj.delta);

          maybeAutoScroll(elLog);
          !userAtBottom && showJumpBtn();
          trimMessagesIfNeeded();

          bumpActivity(0.03 + Math.min(0.05, obj.delta.length * 0.0012));
          if (Math.random() < 0.06) {
            const n = NODES[(Math.random() * NODES.length) | 0];
            spawnPulse(n.x, n.y, 0.55 + activity * 0.35);
          }
        }
      }
    }

    // Flush any buffered prefix chunk (short responses may not reach the length threshold).
    if (!memPrefixDone && memPrefixBuf) {
      typer.push(memPrefixBuf);
      memPrefixBuf = '';
      memPrefixDone = true;
    }

    if (!gotAny) {
      typer.push('[no response]');
      debugHadError = true;
      refreshDebugLed();
    }

    // If the server only sent meta/notice events (no assistant text),
    // we treat the turn as "silent" and remove the empty assistant bubble.
    if (gotAny && !renderedAnyText && !hadError) {
      try { aiMsg.remove(); } catch (e) {}
    }
  } finally {
    // ✅ 无论成功 / 失败 / 中断：都确保收尾 + 关扫光
    typer.finish();

    // ✅ Final UI cleanup: remove noisy prefixes/disclaimers that may have
    // been rendered during streaming.
    try {
      aiContent.textContent = sanitizeAssistantFinalText(aiContent.textContent);
    } catch (e) {}

    glowOff(aiMsg);
    forceIdle();
    aiMsg.classList.remove('streaming');

    bumpActivity(0.06);

    // facts LED/count refresh after any turn (commands like /daily may change counts)
    await fetchFactCounts();

    if (isNearBottom(elLog)) {
      scrollToBottom(elLog);
      userAtBottom = true;
      hideJumpBtn();
    } else {
      userAtBottom = false;
      showJumpBtn();
    }

    trimMessagesIfNeeded();
  }
}

/* ============================================================
   事件绑定（你原来的逻辑：保留）
   ============================================================ */

elSend.onclick = () => {
  const v = elInput.value.trim();
  if (!v) return;
  elInput.value = '';
  sendStream(v);
};

elInput.addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    elSend.click();
  }
});
