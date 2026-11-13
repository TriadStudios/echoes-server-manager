// ESM web page script. Plain JavaScript, no libraries, loaded from the same
// origin. It makes every change through the API (with the CSRF token from
// the page's <meta> tag) and keeps the page current from the API's event
// stream. Reading works without it.
(function () {
  'use strict';

  var meta = document.querySelector('meta[name="csrf-token"]');
  var csrf = meta ? meta.getAttribute('content') : '';
  var flash = document.getElementById('flash');

  function each(sel, fn, root) {
    Array.prototype.forEach.call((root || document).querySelectorAll(sel), fn);
  }

  function say(text, bad) {
    if (!flash) { return; }
    flash.textContent = text;
    flash.hidden = false;
    flash.classList.toggle('bad', !!bad);
  }

  function enc(s) { return encodeURIComponent(s); }

  function inst(name) { return '/api/v1/instances/' + enc(name); }

  // safePath keeps a place to go after a login on this site's pages.
  function safePath(p) {
    if (typeof p !== 'string' || p.charAt(0) !== '/' || p.charAt(1) === '/' || /[\\:]/.test(p) ||
        p.indexOf('/api/') === 0 || p.indexOf('/setup') === 0) {
      return '/';
    }
    return p;
  }

  function toLogin() {
    location.assign('/setup?next=' + enc(location.pathname));
  }

  // call sends one API request. Changes carry JSON and the CSRF token. It
  // resolves with the decoded answer, or rejects with the API's message.
  async function call(method, path, body) {
    var opts = { method: method, credentials: 'same-origin', headers: { Accept: 'application/json' } };
    if (method !== 'GET') {
      opts.headers['Content-Type'] = 'application/json';
      opts.headers['X-CSRF-Token'] = csrf;
      opts.body = JSON.stringify(body || {});
    }
    var resp = await fetch(path, opts);
    var data = null;
    try { data = await resp.json(); } catch (e) { data = null; }
    if (resp.status === 401 && path !== '/api/v1/login' && path !== '/api/v1/setup') {
      toLogin();
    }
    if (!resp.ok) {
      var e = data && data.error ? data.error : {};
      var err = new Error(e.message || ('The request failed (' + resp.status + ').'));
      err.status = resp.status;
      err.code = e.code || '';
      throw err;
    }
    return data;
  }

  // done reloads the page with a fixed notice the server shows.
  function done(code) {
    var u = new URL(location.href);
    u.searchParams.set('done', code);
    location.replace(u.pathname + u.search + u.hash);
  }

  async function busy(el, work) {
    if (el) { el.disabled = true; el.setAttribute('aria-busy', 'true'); }
    try {
      return await work();
    } catch (e) {
      say(e.message, true);
    } finally {
      if (el) { el.disabled = false; el.removeAttribute('aria-busy'); }
    }
  }

  // --- times: shown in the reader's own time zone ---

  function localTime(t) {
    var d = new Date(t.getAttribute('datetime'));
    if (isNaN(d.getTime())) { return; }
    if (!t.title) { t.title = t.textContent; }
    var opts = { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' };
    if (d.getFullYear() !== new Date().getFullYear()) { opts.year = 'numeric'; }
    t.textContent = d.toLocaleString([], opts);
  }
  each('time[datetime]', localTime);

  function timeEl(iso) {
    var t = document.createElement('time');
    t.setAttribute('datetime', iso);
    t.textContent = iso;
    localTime(t);
    return t;
  }

  // --- buttons ---

  var acts = {
    start: async function (b) {
      await call('POST', inst(b.dataset.name) + '/start');
      done('started');
    },
    stop: async function (b) {
      say('Stopping ' + b.dataset.name + '...');
      await call('POST', inst(b.dataset.name) + '/stop');
      done('stopped');
    },
    restart: async function (b) {
      say('Restarting ' + b.dataset.name + '...');
      await call('POST', inst(b.dataset.name) + '/restart');
      done('restarted');
    },
    backup: async function (b) {
      say('Backing up ' + b.dataset.name + '. A running server is first given time for its next world save.');
      await call('POST', inst(b.dataset.name) + '/backup');
      done('backup');
    },
    restore: async function (b) {
      say('Restoring. The server stops and starts again; this can take a while.');
      await call('POST', inst(b.dataset.name) + '/restore', { backup: b.dataset.id });
      done('restored');
    },
    'remove-build': async function (b) {
      await call('DELETE', '/api/v1/builds/' + enc(b.dataset.id));
      done('removed');
    },
    prune: async function () {
      var r = await call('POST', '/api/v1/builds/prune');
      if (!r.removed.length) { say('No build was unused.'); return; }
      done('removed');
    },
    revoke: async function (b) {
      await call('DELETE', '/api/v1/tokens/' + enc(b.dataset.id));
      done('revoked');
    },
    logout: async function () {
      await call('POST', '/api/v1/logout');
      location.assign('/setup');
    }
  };

  document.addEventListener('click', function (ev) {
    var b = ev.target.closest('button[data-act]');
    if (!b || !acts[b.dataset.act]) { return; }
    var question = b.getAttribute('data-confirm');
    if (question && !window.confirm(question)) { return; }
    busy(b, function () { return acts[b.dataset.act](b); });
  });

  // --- forms ---

  function val(f, name) {
    var el = f.elements[name];
    return el ? el.value : '';
  }

  var forms = {
    login: async function (f) {
      await call('POST', '/api/v1/login', { username: val(f, 'username'), password: val(f, 'password') });
      location.assign(safePath(f.dataset.next));
    },
    setup: async function (f) {
      if (val(f, 'password') !== val(f, 'password2')) { throw new Error('The two passwords are not the same.'); }
      var who = { username: val(f, 'username'), password: val(f, 'password') };
      await call('POST', '/api/v1/setup', { code: val(f, 'code').trim(), username: who.username, password: who.password });
      await call('POST', '/api/v1/login', who);
      location.assign(safePath(f.dataset.next));
    },
    update: async function (f) {
      var to = val(f, 'to');
      if (!window.confirm('Update ' + f.dataset.name + ' to build ' + to + '? It is backed up, stopped and started on the new build.')) { return; }
      say('Updating: backing up, stopping, starting build ' + to + ' and waiting until it is ready.');
      await call('POST', inst(f.dataset.name) + '/update', { to: to });
      done('updated');
    },
    policy: async function (f) {
      await call('PATCH', inst(f.dataset.name), { backup: { every: val(f, 'every'), keep: parseInt(val(f, 'keep'), 10) || 0 } });
      done('saved');
    },
    schedule: async function (f) {
      var times = val(f, 'restart_at').split(/[\s,]+/).filter(function (t) { return t !== ''; });
      await call('PATCH', inst(f.dataset.name), { schedule: { restart_at: times } });
      done('saved');
    },
    install: async function (f) {
      var body = { source: val(f, 'source').trim() };
      var sum = val(f, 'sha256').trim();
      if (sum) { body.sha256 = sum.toLowerCase(); }
      say('Checking and installing. A download can take a while.');
      await call('POST', '/api/v1/builds', body);
      done('installed');
    },
    token: async function (f) {
      var t = await call('POST', '/api/v1/tokens', { name: val(f, 'name').trim(), role: val(f, 'role') });
      var box = document.getElementById('new-token');
      document.getElementById('new-token-value').textContent = t.token;
      box.hidden = false;
      f.reset();
      say('Token created. It is in the list after the next reload.');
    }
  };

  document.addEventListener('submit', function (ev) {
    var f = ev.target.closest('form[data-form]');
    if (!f) { return; }
    ev.preventDefault();
    if (!forms[f.dataset.form]) { return; }
    busy(f.querySelector('[type="submit"]'), function () { return forms[f.dataset.form](f); });
  });

  // --- server settings ---

  var sf = document.querySelector('form[data-form="settings"]');
  if (sf) { settings(sf); }

  function settings(f) {
    var list = document.getElementById('pending-list');
    var count = document.getElementById('pending-count');
    var search = document.getElementById('rule-search');
    var onlyChanged = document.getElementById('rule-changed');
    var ruleList = document.getElementById('rule-list');

    function changed(el) {
      if (!el) { return false; }
      if (el.type === 'checkbox') { return String(el.checked) !== el.dataset.orig; }
      return el.value !== el.dataset.orig;
    }
    function fields() { return Array.prototype.slice.call(f.querySelectorAll('[data-orig]')); }
    function rowChanged(row) {
      return row.hasAttribute('data-added') || changed(row.querySelector('input[name^="rule:"]')) ||
        changed(row.querySelector('input[name^="drop:"]'));
    }

    function describe(el) {
      if (el.hasAttribute('data-secret')) { return 'a new value (not shown)'; }
      if (el.type === 'checkbox') { return el.checked ? 'yes' : 'no'; }
      var from = el.dataset.orig === '' ? '(empty)' : el.dataset.orig;
      var to = el.value === '' ? '(empty)' : el.value;
      return from + ' -> ' + to;
    }

    function filterRules() {
      var q = search ? search.value.trim().toLowerCase() : '';
      each('[data-rule]', function (row) {
        var input = row.querySelector('input[type="text"]');
        var text = (row.textContent + ' ' + (input ? input.value : '')).toLowerCase();
        var show = (q === '' || text.indexOf(q) >= 0) && (!onlyChanged || !onlyChanged.checked || rowChanged(row));
        row.hidden = !show;
      }, ruleList);
    }

    function refresh() {
      list.textContent = '';
      var n = 0;
      fields().forEach(function (el) {
        if (!changed(el)) { return; }
        n++;
        var li = document.createElement('li');
        var name = document.createElement('span');
        name.className = 'strong';
        name.textContent = el.dataset.label;
        var what = document.createElement('span');
        what.className = 'mono';
        what.textContent = describe(el);
        li.appendChild(name);
        li.appendChild(what);
        list.appendChild(li);
      });
      each('[data-rule]', function (row) {
        var badge = row.querySelector('.badge.changed');
        if (badge) { badge.hidden = !rowChanged(row); }
      }, ruleList);
      count.textContent = n ? String(n) : 'No';
      each('[data-save]', function (b) { b.disabled = n === 0; });
      filterRules();
    }

    f.addEventListener('input', refresh);
    f.addEventListener('change', refresh);
    if (search) { search.addEventListener('input', filterRules); }
    if (onlyChanged) { onlyChanged.addEventListener('change', filterRules); }

    var addBtn = document.getElementById('add-rule');
    if (addBtn) {
      addBtn.addEventListener('click', function () {
        var keyEl = document.getElementById('new-rule-key');
        var valueEl = document.getElementById('new-rule-value');
        var key = keyEl.value.trim();
        if (!/^[A-Za-z][A-Za-z0-9_]{0,63}$/.test(key)) {
          say('A rule name starts with a letter and has only letters, digits and _.', true);
          return;
        }
        if (f.elements['rule:' + key]) {
          say('Rule ' + key + ' is set already; change its value in the list.', true);
          return;
        }
        var row = document.createElement('div');
        row.className = 'rule';
        row.setAttribute('data-rule', '');
        row.setAttribute('data-added', '');
        var label = document.createElement('label');
        label.className = 'field grow';
        var span = document.createElement('span');
        span.className = 'mono';
        span.textContent = key + ' (new)';
        var input = document.createElement('input');
        input.type = 'text';
        input.className = 'mono';
        input.name = 'rule:' + key;
        input.maxLength = 256;
        input.value = valueEl.value;
        input.defaultValue = valueEl.value;
        input.dataset.orig = '';
        input.dataset.label = 'New rule ' + key;
        label.appendChild(span);
        label.appendChild(input);
        var badge = document.createElement('span');
        badge.className = 'badge changed';
        badge.textContent = 'New';
        row.appendChild(label);
        row.appendChild(badge);
        each('[data-empty]', function (e) { e.remove(); }, ruleList);
        ruleList.appendChild(row);
        keyEl.value = '';
        valueEl.value = '';
        refresh();
      });
    }

    function body() {
      var b = {};
      var secrets = {};
      var rules = {};
      var rulesChanged = false;
      fields().forEach(function (el) {
        var n = el.name;
        if (n.indexOf('rule:') === 0) {
          var key = n.slice(5);
          var drop = f.elements['drop:' + key];
          if (changed(el) || el.closest('[data-added]')) { rulesChanged = true; }
          if (!(drop && drop.checked)) { rules[key] = el.value; }
          return;
        }
        if (n.indexOf('drop:') === 0) {
          if (changed(el)) { rulesChanged = true; }
          return;
        }
        if (!changed(el)) { return; }
        switch (n) {
          case 'max_players': b.max_players = parseInt(el.value, 10) || 0; break;
          case 'listed': b.listed = el.checked; break;
          case 'server_password': if (el.value !== '') { secrets.server_password = el.value; } break;
          case 'admin_password': if (el.value !== '') { secrets.admin_password = el.value; } break;
          case 'clear_server_password': if (el.checked) { secrets.server_password = ''; } break;
          case 'clear_admin_password': if (el.checked) { secrets.admin_password = ''; } break;
          default: b[n] = el.value;
        }
      });
      if (rulesChanged) { b.rules = rules; }
      return { change: b, secrets: secrets };
    }

    async function save(restart) {
      var name = f.dataset.name;
      var parts = body();
      if (Object.keys(parts.change).length) { await call('PATCH', inst(name), parts.change); }
      if (Object.keys(parts.secrets).length) { await call('PUT', inst(name) + '/secrets', parts.secrets); }
      var pill = document.querySelector('[data-state-of]');
      var state = pill ? pill.dataset.state : '';
      if (restart && (state === 'running' || state === 'starting')) {
        say('Saved. Restarting ' + name + '...');
        await call('POST', inst(name) + '/restart');
        done('restarted');
        return;
      }
      done('saved');
    }

    each('[data-save]', function (b) {
      b.addEventListener('click', function () {
        if (b.dataset.save === 'discard') {
          f.reset();
          each('[data-added]', function (row) { row.remove(); }, ruleList);
          refresh();
          return;
        }
        busy(b, function () { return save(b.dataset.save === 'restart'); });
      });
    });
    refresh();
  }

  // --- log screen ---

  var errorRe = /\b(error|fatal|critical)\b/i;
  var warningRe = /\b(warn|warning)\b/i;

  function level(text) {
    if (errorRe.test(text)) { return 'error'; }
    if (warningRe.test(text)) { return 'warning'; }
    return '';
  }

  var logSearch = document.getElementById('log-search');
  var logLevel = document.getElementById('log-level');
  var logFollow = document.getElementById('log-follow');

  function lineShown(div) {
    var lv = logLevel ? logLevel.value : 'all';
    if (lv === 'warning' && !div.classList.contains('warning') && !div.classList.contains('error')) { return false; }
    if (lv === 'error' && !div.classList.contains('error')) { return false; }
    var q = logSearch ? logSearch.value.trim().toLowerCase() : '';
    return q === '' || div.textContent.toLowerCase().indexOf(q) >= 0;
  }

  function filterLog() {
    each('#log-lines .line', function (div) { div.classList.toggle('gone', !lineShown(div)); });
  }
  if (logSearch) { logSearch.addEventListener('input', filterLog); }
  if (logLevel) { logLevel.addEventListener('change', filterLog); }
  if (logFollow) {
    logFollow.addEventListener('change', function () { if (logFollow.checked) { bottom(document.getElementById('log-lines')); } });
  }

  function bottom(box) { if (box) { box.scrollTop = box.scrollHeight; } }
  each('[data-log-of]', bottom);

  function lineEl(text) {
    var div = document.createElement('div');
    var lv = level(text);
    div.className = 'line' + (lv ? ' ' + lv : '');
    if (lv) {
      var tag = document.createElement('span');
      tag.className = 'lvl';
      tag.textContent = lv;
      div.appendChild(tag);
    }
    div.appendChild(document.createTextNode(text));
    return div;
  }

  function addLines(box, lines, replace) {
    var follow = box.id !== 'log-lines' || !logFollow || logFollow.checked;
    if (replace) { box.textContent = ''; }
    each('[data-empty]', function (e) { e.remove(); }, box);
    lines.forEach(function (text) {
      var div = lineEl(text);
      if (box.id === 'log-lines') { div.classList.toggle('gone', !lineShown(div)); }
      box.appendChild(div);
    });
    var keep = parseInt(box.dataset.keep, 10) || 2000;
    while (box.children.length > keep) { box.removeChild(box.firstChild); }
    if (follow) { bottom(box); }
  }

  // --- live updates ---

  function stateWord(s) { return s ? s.charAt(0).toUpperCase() + s.slice(1) : 'Unknown'; }

  function onState(st) {
    each('[data-state-of]', function (el) {
      if (el.dataset.stateOf !== st.name) { return; }
      el.dataset.state = st.state;
      var w = el.querySelector('.word');
      if (w) { w.textContent = stateWord(st.state); }
    });
    each('[data-reason-of]', function (el) {
      if (el.dataset.reasonOf === st.name) { el.textContent = st.reason || stateWord(st.state); }
    });
    var active = st.state === 'running' || st.state === 'starting';
    var idle = st.state === 'stopped' || st.state === 'crashed' || st.state === 'failed';
    each('button[data-when]', function (b) {
      if (b.dataset.name !== st.name) { return; }
      b.hidden = !(b.dataset.when === 'active' ? active : idle);
    });
  }

  function addEvent(list, iso, who, what, max) {
    var row = document.createElement('div');
    row.className = 'row';
    var w = document.createElement('span');
    w.className = 'mono small dim when';
    w.appendChild(timeEl(iso));
    var by = document.createElement('span');
    by.className = 'dim who';
    by.textContent = who;
    var text = document.createElement('span');
    text.className = 'what';
    text.textContent = what;
    row.appendChild(w);
    row.appendChild(by);
    row.appendChild(text);
    each('[data-empty]', function (e) { e.remove(); }, list);
    list.insertBefore(row, list.firstChild);
    while (list.children.length > max) { list.removeChild(list.lastChild); }
  }

  function onEvent(type, e) {
    var data = e.data || {};
    if (type === 'state') {
      onState(data);
      each('[data-events-of]', function (list) {
        if (list.dataset.eventsOf === data.name) { addEvent(list, e.time, 'esm', stateWord(data.state) + ': ' + data.reason, 12); }
      });
    } else if (type === 'log') {
      each('[data-log-of]', function (box) {
        if (box.dataset.logOf === e.instance && box.dataset.source === data.source) { addLines(box, [data.line], false); }
      });
    } else if (type === 'audit') {
      each('[data-activity]', function (list) {
        var what = data.action + (data.target ? ' ' + data.target : '') + (data.ok ? '' : ' (refused)');
        addEvent(list, data.time, data.actor ? data.actor.id : '', what, 8);
      });
    } else if (type === 'build') {
      say('The installed builds changed. Reload the page to see them.');
    }
  }

  // resync catches up after the stream was down: events missed meanwhile
  // are not sent again, so the state and the logs are read anew.
  function resync() {
    call('GET', '/api/v1/instances').then(function (d) {
      d.instances.forEach(function (v) { onState(v.status); });
    }).catch(function () {});
    each('[data-log-of]', function (box) {
      var path = inst(box.dataset.logOf) + '/logs?lines=' + (parseInt(box.dataset.keep, 10) || 200) + '&source=' + enc(box.dataset.source);
      call('GET', path).then(function (d) { addLines(box, d.lines, true); }).catch(function () {});
    });
  }

  // live follows the event stream. EventSource gives up by itself after an
  // HTTP error (401 when the login ended, 502 from a proxy while ESM
  // restarts), so every error closes it and it is opened again here, a
  // little later each time; a 401 goes to the login.
  function live(url) {
    var es = null;
    var wait = 1000;
    var lost = false;
    var timer = 0;
    function retry() {
      clearTimeout(timer);
      timer = setTimeout(open, wait + Math.floor(Math.random() * 500));
      wait = Math.min(wait * 2, 30000);
    }
    function open() {
      es = new EventSource(url);
      es.onopen = function () {
        wait = 1000;
        if (lost) { lost = false; resync(); }
      };
      es.onerror = function () {
        es.close();
        lost = true;
        fetch('/api/v1/session', { credentials: 'same-origin' }).then(function (r) {
          if (r.status === 401) { toLogin(); return; }
          retry();
        }, retry);
      };
      ['state', 'log', 'audit', 'build'].forEach(function (type) {
        es.addEventListener(type, function (m) {
          var e;
          try { e = JSON.parse(m.data); } catch (x) { return; }
          onEvent(type, e);
        });
      });
    }
    window.addEventListener('pagehide', function () { if (es) { es.close(); } clearTimeout(timer); });
    window.addEventListener('pageshow', function (ev) { if (ev.persisted) { lost = true; open(); } });
    open();
  }

  var events = document.body.getAttribute('data-events');
  if (events && window.EventSource) { live('/api/v1/events?' + events); }
})();
