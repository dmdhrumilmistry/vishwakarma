import {
  h, clear, api, ApiError, toast, confirmDialog, chip, until, ago,
  fmtDuration, parseDuration, copy, splitArgs,
} from './core.js';

const app = document.getElementById('app');
const nav = document.getElementById('topnav');

const state = {
  info: null,
  // Cleanup for the current view: timers, terminal sessions.
  teardown: [],
};

function onLeave(fn) { state.teardown.push(fn); }

function leave() {
  for (const fn of state.teardown.splice(0)) {
    try { fn(); } catch { /* ignore */ }
  }
}

function go(hash) {
  if (location.hash === hash) route(); else location.hash = hash;
}

async function route() {
  leave();
  if (!state.info) {
    try {
      state.info = await api('GET', '/info');
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) return loginView();
      return fatal(e);
    }
  }
  renderNav();
  const path = location.hash.replace(/^#/, '') || '/';
  let m;
  if (path === '/') return listView();
  if (path === '/new') return newView();
  if ((m = path.match(/^\/s\/([a-z0-9-]+)(?:\/(\w+))?$/))) return detailView(m[1], m[2] || 'overview');
  go('#/');
}

function fatal(e) {
  clear(app).appendChild(h('div', { class: 'panel notice-bad' },
    h('h2', {}, 'Something went wrong'),
    h('p', {}, e.message || String(e))));
}

function renderNav() {
  const i = state.info;
  clear(nav);
  nav.append(
    h('span', { class: 'muted hide-sm' }, i.user, i.admin ? ' (admin)' : ''),
    i.authMode === 'password'
      ? h('button', { class: 'btn btn-sm btn-ghost', type: 'button', onclick: logout }, 'Sign out')
      : null,
  );
}

async function logout() {
  try { await api('POST', '/logout'); } catch { /* ignore */ }
  state.info = null;
  clear(nav);
  loginView();
}

// ---------------------------------------------------------------- login

function loginView() {
  clear(nav);
  const pw = h('input', { type: 'password', id: 'pw', autocomplete: 'current-password', required: true });
  const err = h('p', { class: 'notice notice-bad', hidden: true });
  const btn = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Sign in');
  const form = h('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      btn.disabled = true;
      err.hidden = true;
      try {
        await api('POST', '/login', { password: pw.value });
        state.info = null;
        route();
      } catch (ex) {
        err.textContent = ex.status === 401 ? 'That password is not right. Try again.' : ex.message;
        err.hidden = false;
        btn.disabled = false;
        pw.select();
      }
    },
  },
  h('label', { class: 'field', for: 'pw' }, h('span', {}, 'Admin password'), pw),
  err, btn);
  clear(app).appendChild(h('div', { class: 'panel login' },
    h('h1', {}, 'Sign in'),
    h('p', { class: 'muted' }, 'Sign in to create and manage test sandboxes.'),
    form));
  pw.focus();
}

const KIND_LABEL = { container: 'Container', vm: 'VM', macos: 'macOS' };

function kindTags(s) {
  const tpl = s.kind === 'container' ? (s.template || '') : '';
  const label = tpl.startsWith('android') ? 'Android'
    : tpl === 'macos-linux' ? 'macOS on Linux'
      : (KIND_LABEL[s.kind] || s.kind);
  return [
    h('span', { class: 'tag' }, label),
    s.simulated ? h('span', { class: 'tag', title: 'From the agent simulator: there is no real macOS guest' }, 'Simulated') : null,
  ];
}

// ---------------------------------------------------------------- list

function listView() {
  const i = state.info;
  const body = h('div', {}, h('p', { class: 'muted' }, 'Loading sandboxes...'));
  const sub = h('p', { class: 'muted sub small' });
  clear(app).append(
    h('div', { class: 'page-head' },
      h('div', {}, h('h1', {}, 'Sandboxes'), sub),
      h('a', { class: 'btn btn-primary', href: '#/new' }, 'New sandbox')),
    body);

  const load = async () => {
    let items;
    try {
      items = (await api('GET', '/sandboxes')).items;
    } catch (e) {
      clear(body).appendChild(h('div', { class: 'notice notice-bad' }, e.message));
      return;
    }
    const mine = items.filter((s) => s.owner === i.user).length;
    sub.textContent = i.admin
      ? `${items.length} total, all users`
      : `${mine} of ${i.policy.maxSandboxesPerUser || 'unlimited'} used`;
    clear(body).appendChild(items.length ? table(items) : empty());
  };
  load();
  const t = setInterval(load, 5000);
  onLeave(() => clearInterval(t));
}

function empty() {
  return h('div', { class: 'panel empty' },
    h('h2', {}, 'No sandboxes yet'),
    h('p', { class: 'muted' }, 'Spin up a throwaway container or VM to test an app or an endpoint agent. It deletes itself when its time runs out.'),
    h('a', { class: 'btn btn-primary', href: '#/new' }, 'Create your first sandbox'));
}

function table(items) {
  const admin = state.info.admin;
  return h('div', { class: 'panel sb-list-panel' },
    h('table', { class: 'sb-table' },
      h('thead', {}, h('tr', {},
        h('th', {}, 'Name'),
        h('th', {}, 'Status'),
        h('th', { class: 'hide-sm' }, 'Image'),
        admin ? h('th', { class: 'hide-sm' }, 'Owner') : null,
        h('th', { class: 'hide-sm' }, 'Size'),
        h('th', {}, 'Expires in'))),
      h('tbody', {}, items.map((s) => h('tr', { onclick: () => go(`#/s/${s.name}`) },
        h('td', {},
          h('a', { class: 'sb-name', href: `#/s/${s.name}` }, s.name), ' ',
          kindTags(s)),
        h('td', {}, chip(s.status)),
        h('td', { class: 'hide-sm' }, h('span', { class: 'sb-image' }, s.image)),
        admin ? h('td', { class: 'hide-sm' }, s.owner) : null,
        h('td', { class: 'hide-sm small' }, `${s.cpu} CPU, ${s.memory}`),
        h('td', {}, until(s.expiresAt)))))));
}

// ---------------------------------------------------------------- create

function ttlChoices(p) {
  const max = parseDuration(p.maxTTL);
  const opts = ['30m', '1h', '2h', '4h', '8h', '24h', '72h', '168h']
    .filter((d) => parseDuration(d) <= max);
  const def = fmtDuration(p.defaultTTL);
  if (!opts.includes(def)) opts.push(def);
  return { opts: opts.sort((a, b) => parseDuration(a) - parseDuration(b)), def };
}

function label(d) {
  const ms = parseDuration(d);
  if (ms >= 86400e3 && ms % 86400e3 === 0) return `${ms / 86400e3} day${ms === 86400e3 ? '' : 's'}`;
  if (ms >= 3600e3 && ms % 3600e3 === 0) return `${ms / 3600e3} hour${ms === 3600e3 ? '' : 's'}`;
  return `${ms / 60e3} minutes`;
}

function suggestName(base) {
  const suffix = Math.random().toString(36).slice(2, 6);
  const clean = (base || 'sandbox').toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 30);
  return `${clean || 'sandbox'}-${suffix}`;
}

function newView() {
  const i = state.info;
  const p = i.policy;
  const form = { kind: 'container', template: null, custom: false };
  const ttl = ttlChoices(p);

  const kindSeg = h('div', { class: 'seg', role: 'group', 'aria-label': 'Kind' });
  const tplGrid = h('div', { class: 'tpl-grid' });
  const customBox = h('div', { class: 'stack', hidden: true });
  const vmBox = h('div', { class: 'stack', hidden: true });
  const containerBox = h('div', { class: 'stack' });
  const errBox = h('div', { class: 'notice notice-bad', hidden: true });

  const name = h('input', { id: 'f-name', required: true, maxlength: 40, pattern: '[a-z0-9]([-a-z0-9]*[a-z0-9])?', autocomplete: 'off', spellcheck: 'false' });
  const image = h('input', { id: 'f-image', placeholder: 'nginx:1.29 or ghcr.io/you/app:dev', autocomplete: 'off', spellcheck: 'false' });
  const command = h('input', { id: 'f-cmd', placeholder: 'Leave empty to use the image entrypoint', autocomplete: 'off', spellcheck: 'false' });
  const cpu = h('input', { id: 'f-cpu', placeholder: p.defaults.cpu });
  const memory = h('input', { id: 'f-mem', placeholder: p.defaults.memory });
  const diskOn = h('input', { type: 'checkbox', id: 'f-disk-on' });
  const disk = h('input', { id: 'f-disk', placeholder: p.defaults.disk, disabled: true });
  const ttlSel = h('select', { id: 'f-ttl' }, ttl.opts.map((d) => h('option', { value: d, selected: d === ttl.def }, label(d))));
  const ports = h('input', { id: 'f-ports', placeholder: 'e.g. 80, 443, 8080', autocomplete: 'off' });
  const expose = h('select', { id: 'f-expose' },
    h('option', { value: 'cluster' }, 'Inside the cluster only'),
    p.allowNodePort ? h('option', { value: 'nodeport' }, 'On every node (NodePort)') : null);
  const env = h('textarea', { id: 'f-env', placeholder: 'KEY=value, one per line', spellcheck: 'false' });
  const sshKey = h('textarea', { id: 'f-ssh', placeholder: 'ssh-ed25519 AAAA... you@laptop', spellcheck: 'false' });
  const privileged = h('input', { type: 'checkbox', id: 'f-priv' });
  const submit = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Create sandbox');

  diskOn.addEventListener('change', () => {
    disk.disabled = !diskOn.checked;
    diskField.hidden = !diskOn.checked;
    if (diskOn.checked) disk.focus();
  });

  const kinds = [['container', 'Container'], ['vm', 'Virtual machine'], ['macos', 'macOS']];
  const unavailable = {
    vm: i.vms ? null : 'KubeVirt is not installed on this cluster',
    macos: i.macos ? null : 'No Mac host agent is configured or reachable',
  };
  const renderKinds = () => {
    clear(kindSeg);
    for (const [k, text] of kinds) {
      const disabled = Boolean(unavailable[k]);
      kindSeg.appendChild(h('button', {
        type: 'button', 'aria-pressed': String(form.kind === k), disabled,
        title: unavailable[k],
        onclick: () => { form.kind = k; form.template = null; form.custom = false; renderKinds(); renderTemplates(); },
      }, text));
    }
  };

  const pick = (t, custom) => {
    form.template = t;
    form.custom = custom;
    if (!name.dataset.touched) name.value = suggestName(custom ? 'app' : t.name.replace(/-vm$/, ''));
    renderTemplates();
  };

  const renderTemplates = () => {
    clear(tplGrid);
    const list = i.templates.filter((t) => t.kind === form.kind);
    if (!form.template && !form.custom) {
      if (list.length) pick(list[0], false); else if (p.allowCustomImages) pick(null, true);
      return;
    }
    for (const t of list) {
      tplGrid.appendChild(h('button', {
        type: 'button', class: 'tpl', 'aria-pressed': String(!form.custom && form.template === t), onclick: () => pick(t, false),
      },
      h('strong', {}, t.displayName),
      h('span', { class: 'sb-image' }, t.image),
      t.description ? h('span', { class: 'desc' }, t.description) : null));
    }
    if (p.allowCustomImages) {
      tplGrid.appendChild(h('button', {
        type: 'button', class: 'tpl', 'aria-pressed': String(form.custom), onclick: () => pick(null, true),
      },
      h('strong', {}, form.kind === 'container' ? 'Custom image' : 'Custom disk image'),
      h('span', { class: 'desc' }, {
        vm: 'Any KubeVirt containerDisk image',
        macos: 'Any Tart macOS image (an OCI reference)',
      }[form.kind] || 'Your own app image, with ports and environment')));
    }
    customBox.hidden = !form.custom;
    command.closest('.field').hidden = form.kind !== 'container';
    vmBox.hidden = form.kind === 'container';
    vmNote.textContent = form.kind === 'macos'
      ? 'Runs on a Mac host. The terminal is an SSH session; the screen is reachable over VNC. The first pull of a macOS image can take a long time.'
      : 'The VM disk resets when the VM stops. Use the terminal for the serial console, or SSH through an exposed port.';
    exposeField.hidden = form.kind === 'macos';
    containerBox.hidden = form.kind !== 'container';
    const r = (form.template && form.template.resources) || {};
    cpu.placeholder = r.cpu || p.defaults.cpu;
    memory.placeholder = r.memory || p.defaults.memory;
    ports.placeholder = { vm: '22 (default)', macos: 'SSH and VNC are always forwarded' }[form.kind] || 'e.g. 80, 443, 8080';
  };

  name.addEventListener('input', () => { name.dataset.touched = '1'; });

  const vmNote = h('p', { class: 'muted small' });
  const field = (id, text, input, hint) => h('label', { class: 'field', for: id },
    h('span', {}, text), input, hint ? h('span', { class: 'hint' }, hint) : null);

  const exposeField = field('f-expose', 'Reachable from', expose,
    p.allowNodePort ? 'NodePort opens a high port on every node of the cluster.' : null);

  const parsePorts = () => {
    const raw = ports.value.trim();
    if (!raw) return undefined;
    return raw.split(/[\s,]+/).filter(Boolean).map((x) => {
      const n = Number(x);
      if (!Number.isInteger(n) || n < 1 || n > 65535) throw new Error(`"${x}" is not a port number`);
      return n;
    });
  };
  const parseEnv = () => {
    const out = {};
    for (const line of env.value.split('\n')) {
      const l = line.trim();
      if (!l || l.startsWith('#')) continue;
      const eq = l.indexOf('=');
      if (eq < 1) throw new Error(`"${l}" is not KEY=value`);
      out[l.slice(0, eq).trim()] = l.slice(eq + 1);
    }
    return Object.keys(out).length ? out : undefined;
  };

  const onSubmit = async (e) => {
    e.preventDefault();
    errBox.hidden = true;
    let spec;
    try {
      spec = {
        name: name.value.trim(),
        kind: form.kind,
        template: form.custom ? undefined : form.template.name,
        image: form.custom ? image.value.trim() : undefined,
        command: form.custom && form.kind === 'container' && command.value.trim() ? splitArgs(command.value.trim()) : undefined,
        cpu: cpu.value.trim() || undefined,
        memory: memory.value.trim() || undefined,
        disk: form.kind === 'container' && diskOn.checked ? (disk.value.trim() || p.defaults.disk) : undefined,
        ttl: ttlSel.value,
        ports: parsePorts(),
        env: form.kind === 'container' ? parseEnv() : undefined,
        expose: form.kind === 'macos' ? undefined : expose.value,
        sshKey: form.kind !== 'container' && sshKey.value.trim() ? sshKey.value.trim() : undefined,
        privileged: form.kind === 'container' && privileged.checked ? true : undefined,
      };
      if (form.custom && !spec.image) throw new Error('Enter an image for the custom sandbox.');
    } catch (ex) {
      errBox.textContent = ex.message;
      errBox.hidden = false;
      return;
    }
    submit.disabled = true;
    submit.textContent = 'Creating...';
    try {
      const s = await api('POST', '/sandboxes', spec);
      toast(`Created ${s.name}`);
      go(`#/s/${s.name}`);
    } catch (ex) {
      errBox.textContent = ex.message;
      errBox.hidden = false;
      submit.disabled = false;
      submit.textContent = 'Create sandbox';
      errBox.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    }
  };

  clear(app).append(
    h('div', { class: 'page-head' },
      h('div', {}, h('h1', {}, 'New sandbox'),
        h('p', { class: 'muted sub small' }, `Deleted automatically when its time runs out (at most ${label(fmtDuration(p.maxTTL))}).`)),
      h('a', { class: 'btn', href: '#/' }, 'Cancel')),
    h('form', { onsubmit: onSubmit, novalidate: true },
      h('section', { class: 'panel stack' },
        h('h2', {}, 'What to run'),
        kindSeg,
        i.vms ? null : h('p', { class: 'muted small' }, 'Virtual machines need KubeVirt on the cluster. Ask your administrator to install it.'),
        tplGrid,
        customBox),
      h('section', { class: 'panel stack' },
        h('h2', {}, 'Settings'),
        h('div', { class: 'grid-2' },
          field('f-name', 'Name', name, 'Lowercase letters, digits and dashes.'),
          field('f-ttl', 'Delete after', ttlSel)),
        h('div', { class: 'grid-2' },
          field('f-cpu', 'CPU', cpu, `Cores, e.g. 500m or 2. Up to ${p.limits.cpu}.`),
          field('f-mem', 'Memory', memory, `e.g. 512Mi or 2Gi. Up to ${p.limits.memory}.`)),
        containerBox,
        vmBox),
      h('section', { class: 'panel stack' },
        h('h2', {}, 'Network'),
        h('div', { class: 'grid-2' },
          field('f-ports', 'Ports to expose', ports, 'TCP ports, separated by commas.'),
          exposeField),
        p.isolated ? h('p', { class: 'muted small' }, 'Sandboxes are isolated: they accept traffic only on these ports and cannot reach other workloads in the cluster.') : null),
      errBox,
      h('div', { class: 'form-actions' }, h('a', { class: 'btn', href: '#/' }, 'Cancel'), submit)));

  customBox.append(
    field('f-image', 'Image', image, 'For VMs, a KubeVirt containerDisk image.'),
    field('f-cmd', 'Command', command, 'Overrides the entrypoint. Quote arguments that contain spaces.'));
  const diskField = field('f-disk', 'Volume size', disk, `Up to ${p.limits.disk}.`);
  diskField.hidden = true;
  // h() drops null children; Element.append would print "null".
  containerBox.appendChild(h('div', { class: 'stack' },
    h('label', { class: 'check', for: 'f-disk-on' }, diskOn,
      h('span', {}, 'Persistent ', h('code', {}, '/data'), ' volume', h('span', { class: 'hint muted small' }, ' (kept across stop and start)'))),
    diskField,
    field('f-env', 'Environment variables', env),
    p.allowPrivileged ? h('label', { class: 'check', for: 'f-priv' }, privileged,
      h('span', {}, 'Privileged', h('br'), h('span', { class: 'muted small' }, 'Full access to the node kernel. Needed by some endpoint agents; never use with untrusted images.'))) : null));
  vmBox.append(
    field('f-ssh', 'SSH public key (optional)', sshKey, 'Added for the login user. A password is generated either way.'),
    vmNote);

  renderKinds();
  renderTemplates();
}

// ---------------------------------------------------------------- detail

function detailView(name, tab) {
  const head = h('div', { class: 'page-head' });
  const tabs = h('div', { class: 'tabs', role: 'tablist' });
  const body = h('div', {});
  clear(app).append(
    h('p', { class: 'small' }, h('a', { href: '#/' }, 'All sandboxes')),
    head, tabs, body);

  let current = null;
  let shownTab = null;

  const renderHead = (s) => {
    const busy = s.status === 'Terminating';
    const stopped = s.status === 'Stopped';
    const ext = h('select', { class: 'btn-sm', 'aria-label': 'Extend by' },
      ttlChoices(state.info.policy).opts.map((d) => h('option', { value: d }, label(d))));
    clear(head).append(
      h('div', {},
        h('div', { class: 'row' }, h('h1', {}, s.name), chip(s.status), kindTags(s)),
        h('p', { class: 'muted sub small' },
          friendly(s.message),
          s.emulated ? 'Runs under software emulation (no node has /dev/kvm): booting and installing take a long time. ' : '',
          `Deletes itself in ${until(s.expiresAt)}.`)),
      h('div', { class: 'row' },
        stopped
          ? h('button', { class: 'btn', type: 'button', disabled: busy, onclick: () => act('start') }, 'Start')
          : h('button', { class: 'btn', type: 'button', disabled: busy, onclick: () => act('stop') }, 'Stop'),
        h('span', { class: 'row nowrap' }, ext,
          h('button', { class: 'btn btn-sm', type: 'button', disabled: busy, onclick: () => extend(ext.value) }, 'Extend')),
        h('button', { class: 'btn btn-danger', type: 'button', disabled: busy, onclick: del }, 'Delete')));
  };

  const renderTabs = (s) => {
    const list = [['overview', 'Overview']];
    if (s.screen) list.push(['screen', 'Screen']);
    list.push(['terminal', s.kind === 'vm' ? 'Console' : 'Terminal']);
    if (s.kind === 'container') list.push(['logs', 'Logs']);
    clear(tabs);
    for (const [k, text] of list) {
      tabs.appendChild(h('button', {
        type: 'button', role: 'tab', 'aria-selected': String(tab === k),
        onclick: () => { location.hash = `#/s/${name}/${k}`; },
      }, text));
    }
  };

  const act = async (what) => {
    try {
      current = await api('POST', `/sandboxes/${name}/${what}`);
      toast(what === 'stop' ? 'Stopping' : 'Starting');
      refresh();
    } catch (e) { toast(e.message, true); }
  };
  const extend = async (ttl) => {
    try {
      current = await api('POST', `/sandboxes/${name}/extend`, { ttl });
      toast(`Now deletes itself in ${until(current.expiresAt)}`);
      refresh();
    } catch (e) { toast(e.message, true); }
  };
  const del = async () => {
    const ok = await confirmDialog(`Delete ${name}?`, 'The sandbox and its data are removed now. This cannot be undone.', 'Delete sandbox');
    if (!ok) return;
    try {
      await api('DELETE', `/sandboxes/${name}`);
      toast(`Deleting ${name}`);
      go('#/');
    } catch (e) { toast(e.message, true); }
  };

  const refresh = async () => {
    try {
      current = await api('GET', `/sandboxes/${name}`);
    } catch (e) {
      if (e.status === 404) {
        clear(head);
        clear(tabs);
        clear(body).appendChild(h('div', { class: 'panel empty' },
          h('h2', {}, 'Sandbox not found'),
          h('p', { class: 'muted' }, 'It may have expired or been deleted.'),
          h('a', { class: 'btn', href: '#/' }, 'Back to sandboxes')));
        leave();
        return;
      }
      toast(e.message, true);
      return;
    }
    renderHead(current);
    renderTabs(current);
    if (shownTab !== tab) {
      shownTab = tab;
      renderBody(current);
    } else if (tab === 'overview') {
      renderBody(current);
    }
  };

  const renderBody = (s) => {
    if (tab === 'terminal') return terminalTab(body, s);
    if (tab === 'screen' && s.screen) return screenTab(body, s);
    if (tab === 'logs' && s.kind === 'container') return logsTab(body, s);
    return overviewTab(body, s);
  };

  refresh();
  const t = setInterval(refresh, 5000);
  onLeave(() => clearInterval(t));
}

function overviewTab(body, s) {
  const kv = (k, v) => [h('dt', {}, k), h('dd', {}, v === undefined || v === '' ? '-' : v)];
  const eps = s.endpoints.length
    ? h('ul', { class: 'ep-list' }, s.endpoints.map((e) => h('li', {},
      h('span', {},
        h('strong', {}, e.name ? `${e.name.toUpperCase()} (${e.port})` : `${e.port}/${e.protocol.toLowerCase()}`), ' ',
        h('code', {}, e.address),
        endpointHint(s, e)),
      h('button', { class: 'btn btn-sm', type: 'button', onclick: () => copy(e.address) }, 'Copy'))))
    : h('p', { class: 'muted small' }, 'No ports exposed.');

  const left = h('section', { class: 'panel' },
    h('h2', {}, 'Details'),
    h('dl', { class: 'kv' },
      kv('Image', h('span', { class: 'sb-image' }, s.image)),
      kv('Template', s.template),
      kv('Owner', s.owner),
      kv('Created', ago(s.createdAt)),
      kv('Expires', new Date(s.expiresAt).toLocaleString()),
      kv('CPU', s.cpu),
      kv('Memory', s.memory),
      s.kind === 'container' ? kv('Volume', s.disk ? `${s.disk} at /data` : 'none') : null,
      s.privileged ? kv('Privileged', 'yes') : null,
      kv(s.kind === 'macos' ? 'Mac host' : 'Node', s.node),
      kv(s.kind === 'container' ? 'Sandbox IP' : 'Guest IP', s.ip)));

  const right = h('div', { class: 'stack' },
    h('section', { class: 'panel' }, h('h2', {}, 'Endpoints'), eps),
    s.kind !== 'container' || templateLogin(s) ? credentials(s) : null);

  clear(body).appendChild(h('div', { class: 'grid-2' }, left, right));
}

// endpointHint suggests the client command for well-known ports.
function endpointHint(s, e) {
  let cmd = null;
  if (e.port === 5555 && s.template && s.template.startsWith('android')) cmd = `adb connect ${e.address}`;
  if (e.name === 'vnc') cmd = `open vnc://${e.address}`;
  if (!cmd) return null;
  return h('span', { class: 'ep-hint' }, h('code', {}, cmd),
    e.nodePort || s.kind === 'macos' ? null : h('span', { class: 'muted' }, ' (expose with NodePort to reach it from outside)'));
}

// friendly explains scheduler messages people hit often.
function friendly(msg) {
  if (!msg) return '';
  if (msg.includes('devices.kubevirt.io/kvm')) {
    return 'Waiting for a node with /dev/kvm (hardware virtualization). No node has it free right now. ';
  }
  if (msg.includes('Insufficient memory') || msg.includes('Insufficient cpu')) {
    return 'Waiting for a node with enough free memory or CPU. ';
  }
  return `${msg}. `;
}

// templateLogin reports whether a container's image ships a fixed login.
function templateLogin(s) {
  const t = state.info.templates.find((x) => x.name === s.template);
  return Boolean(t && t.user && t.image === s.image);
}

// Revealed logins survive the overview's periodic re-render.
const revealed = new Map();

function credentials(s) {
  const pass = h('span', { class: 'secret' }, '****************');
  let shown = revealed.get(s.name) || null;
  const reveal = h('button', {
    class: 'btn btn-sm', type: 'button',
    onclick: async () => {
      if (!shown) {
        try { shown = await api('GET', `/sandboxes/${s.name}/credentials`); } catch (e) { toast(e.message, true); return; }
        revealed.set(s.name, shown);
      }
      pass.textContent = shown.password || '(set by the template)';
      vncPass.textContent = shown.vncPassword || '(none)';
      reveal.hidden = true;
      copyBtn.hidden = !shown.password;
    },
  }, 'Show');
  const copyBtn = h('button', { class: 'btn btn-sm', type: 'button', hidden: true, onclick: () => copy(shown.password) }, 'Copy');
  const ssh = s.endpoints.find((e) => e.name === 'ssh') || s.endpoints.find((e) => e.port === 22);
  const vncPass = h('span', { class: 'secret' }, '********');
  const vncRow = s.kind === 'macos' ? [h('dt', {}, 'VNC password'), h('dd', {}, vncPass)] : null;
  let hint = null;
  if (ssh && ssh.nodePort) {
    const host = ssh.address.split(':')[0];
    hint = h('p', { class: 'small' }, 'SSH: ', h('code', {}, `ssh ${s.user}@${host} -p ${ssh.nodePort}`));
  } else if (s.kind === 'macos') {
    hint = h('p', { class: 'muted small' }, 'Endpoints appear once the VM is running.');
  }
  if (shown) reveal.click(); // already revealed before this re-render
  return h('section', { class: 'panel' },
    h('h2', {}, 'Login'),
    h('dl', { class: 'kv' },
      h('dt', {}, 'User'), h('dd', {}, h('code', {}, s.user || (state.info.templates.find((x) => x.name === s.template) || {}).user || '-')),
      h('dt', {}, 'Password'), h('dd', { class: 'row' }, pass, reveal, copyBtn),
      vncRow),
    hint,
    h('p', { class: 'muted small' }, {
      macos: 'The image password is replaced with this one on first boot.',
      container: 'This login is built into the image.',
    }[s.kind] || 'Cloud-init sets the login on first boot, which can take a few minutes.'));
}

function terminalTab(body, s) {
  const status = h('span', { class: 'term-status' });
  const host = h('div', { class: 'term' });
  let close = null;
  const connect = async () => {
    if (close) close();
    clear(host);
    const { openTerminal } = await import('./terminal.js');
    close = openTerminal(host, s.name, (t) => { status.textContent = t; });
  };
  const reconnect = h('button', { class: 'btn btn-sm', type: 'button', onclick: connect }, 'Reconnect');
  clear(body).append(
    h('div', { class: 'term-bar' }, status, reconnect),
    h('div', { class: 'term-wrap' }, host),
    s.kind === 'vm' ? h('p', { class: 'muted small' }, 'This is the serial console. Log in with the user and password from the Overview tab.') : null,
    s.kind === 'macos' ? h('p', { class: 'muted small' }, `An SSH session into the VM as ${s.user}.`) : null);
  if (s.status === 'Running') {
    connect();
  } else {
    status.textContent = `The sandbox is ${s.status.toLowerCase()}. Start it, then reconnect.`;
  }
  state.teardown.push(() => { if (close) close(); });
}

function screenTab(body, s) {
  const status = h('span', { class: 'term-status' });
  const clip = h('textarea', {
    class: 'clip', rows: 2, spellcheck: 'false', 'aria-label': 'Clipboard',
    placeholder: 'Text copied on the device shows up here. Type or paste here, then Paste into device.',
  });
  const host = h('div', { class: 'screen' });
  const wrap = h('div', { class: 'screen-wrap' }, host);
  let session = null;
  const password = async () => {
    const c = revealed.get(s.name) || await api('GET', `/sandboxes/${s.name}/credentials`);
    revealed.set(s.name, c);
    return c.vncPassword || '';
  };
  const connect = async () => {
    if (session) session.close();
    clear(host);
    const { openScreen } = await import('./screen.js');
    session = openScreen(host, s.name, (t) => { status.textContent = t; }, password, (text) => {
      clip.value = text;
      toast('Copied on the device: it is in the clipboard box below');
    }, { android });
  };
  const android = (s.template || '').startsWith('android');
  const btn = (text, title, fn) => h('button', { class: 'btn btn-sm', type: 'button', title, onclick: () => session && fn() }, text);
  const keys = android
    ? [btn('Back', 'Android back (Alt+B)', () => session.altKey('b')),
      btn('Home', 'Android home (Alt+H)', () => session.altKey('h')),
      btn('Recents', 'Recent apps (Alt+S)', () => session.altKey('s'))]
    : [btn('Ctrl+Alt+Del', 'Send Ctrl+Alt+Del', () => session.ctrlAltDel())];
  clear(body).append(
    h('div', { class: 'term-bar' }, status,
      h('div', { class: 'row' }, keys,
        h('button', { class: 'btn btn-sm', type: 'button', onclick: () => wrap.requestFullscreen && wrap.requestFullscreen() }, 'Full screen'),
        h('button', { class: 'btn btn-sm', type: 'button', onclick: connect }, 'Reconnect'))),
    wrap,
    h('div', { class: 'clip-bar' },
      clip,
      h('div', { class: 'row' },
        h('button', { class: 'btn btn-sm', type: 'button', onclick: () => session && clip.value && session.paste(clip.value) }, 'Paste into device'),
        h('button', { class: 'btn btn-sm', type: 'button', onclick: () => clip.value && copy(clip.value) }, 'Copy'))),
    h('p', { class: 'muted small' }, 'Ctrl+V in the screen pastes your clipboard into the device. Text you copy on the device appears in the box above.'),
    h('p', { class: 'muted small' }, android
      ? 'Click and type to use the device. The screen appears once Android has booted, about a minute after start.'
      : s.kind === 'vm'
        ? 'The VM display. Text-only guests show their console here too.'
        : 'The display of the sandbox, over VNC.'));
  if (s.status === 'Running') {
    connect();
  } else {
    status.textContent = `The sandbox is ${s.status.toLowerCase()}. Start it, then reconnect.`;
  }
  state.teardown.push(() => { if (session) session.close(); });
}

function logsTab(body, s) {
  const pre = h('pre', { class: 'logs' }, 'Loading...');
  const load = async () => {
    try {
      const text = await api('GET', `/sandboxes/${s.name}/logs?tail=1000`);
      pre.textContent = text || '(no output yet)';
      pre.scrollTop = pre.scrollHeight;
    } catch (e) {
      pre.textContent = e.message;
    }
  };
  clear(body).append(
    h('div', { class: 'term-bar' },
      h('span', { class: 'term-status' }, 'Last 1000 lines of the main process'),
      h('button', { class: 'btn btn-sm', type: 'button', onclick: load }, 'Refresh')),
    pre);
  load();
}

window.addEventListener('hashchange', route);
route();
