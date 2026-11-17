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

  // sessionEnded: start
  // sessionEnded says whether an answer of the API to path means that the
  // login is gone: a 401 (but not from the login or the setup, which answer
  // it for a wrong password or code), or a 429 from /api/v1/session, which
  // only the limit on failed authentications of an address answers, and
  // only to a caller without a valid login. Other 429s (a busy download,
  // the login's own limit) are not.
  function sessionEnded(path, status) {
    if (status === 401) { return path !== '/api/v1/login' && path !== '/api/v1/setup'; }
    return status === 429 && path === '/api/v1/session';
  }
  // sessionEnded: end

  // call sends one API request. Changes carry JSON and the CSRF token, and
  // the further headers given (an object, such as If-Match). It resolves
  // with the decoded answer, or rejects with the API's message, its code
  // and the HTTP status.
  async function call(method, path, body, headers) {
    var opts = { method: method, credentials: 'same-origin', headers: { Accept: 'application/json' } };
    if (headers) {
      Object.keys(headers).forEach(function (k) { opts.headers[k] = headers[k]; });
    }
    if (method !== 'GET') {
      opts.headers['Content-Type'] = 'application/json';
      opts.headers['X-CSRF-Token'] = csrf;
      opts.body = JSON.stringify(body || {});
    }
    var resp = await fetch(path, opts);
    var data = null;
    try { data = await resp.json(); } catch (e) { data = null; }
    if (sessionEnded(path, resp.status)) {
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

  // awaitEsm: start
  // After a self-update ESM ends and its service manager starts the new
  // binary, so the page waits for that: it looks at GET /api/v1/box, the
  // route that names the running version, every WAIT_EVERY ms, and gives up
  // WAIT_FOR ms after it began.
  var WAIT_EVERY = 2000;
  var WAIT_FOR = 60000;

  // updateStep says what one look at /api/v1/box means to a page that waits
  // for ESM to run version want: 'back' when ESM answers with that version,
  // or answers without the login. Sessions are kept in memory only, so a
  // restarted ESM has none (401, or 429 for an address limited for failed
  // authentications), while the old one keeps them until it ends. Anything
  // else is 'wait': no answer yet, an error from a proxy in front of ESM,
  // or the old version still answering.
  function updateStep(status, box, want) {
    if (status === 200) { return box && box.version === want ? 'back' : 'wait'; }
    return sessionEnded('/api/v1/box', status) || status === 429 ? 'back' : 'wait';
  }

  // awaitEsm looks with io.look() (which resolves {status, box}, or rejects
  // when nothing answers) until updateStep says 'back' or the time is over.
  // io brings the clock and the timers (now(), later(fn, ms), cancel(h)), so
  // that a test can run it without a page. It gives {done, stop}: done
  // resolves 'back', 'late' (the time is over; the last look came after it)
  // or 'stopped' (stop() was called, for a page that goes away).
  function awaitEsm(want, io) {
    var began = io.now();
    var timer = null;
    var over = false;
    var finish = null;
    var done = new Promise(function (resolve) { finish = resolve; });
    function end(how) {
      if (over) { return; }
      over = true;
      io.cancel(timer);
      finish(how);
    }
    function look() {
      io.look().then(function (a) {
        return updateStep(a.status, a.box, want);
      }).catch(function () {
        return 'wait';
      }).then(function (step) {
        if (over) { return; }
        if (step === 'back') {
          end('back');
        } else if (io.now() - began >= WAIT_FOR) {
          end('late');
        } else {
          timer = io.later(look, WAIT_EVERY);
        }
      });
    }
    timer = io.later(look, WAIT_EVERY);
    return { done: done, stop: function () { end('stopped'); } };
  }
  // awaitEsm: end

  // lookAtBox reads GET /api/v1/box with its own request, not call(): the
  // answers that matter here (401 from a restarted ESM, 502 from a proxy)
  // must not send the page anywhere by themselves. A look that gets no
  // answer in 5 seconds is given up, so one hanging connection cannot hold
  // the wait.
  async function lookAtBox() {
    var ctl = window.AbortController ? new AbortController() : null;
    var timer = ctl ? setTimeout(function () { ctl.abort(); }, 5000) : 0;
    try {
      var resp = await fetch('/api/v1/box', {
        credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' },
        signal: ctl ? ctl.signal : undefined
      });
      var box = null;
      try { box = await resp.json(); } catch (e) { box = null; }
      return { status: resp.status, box: box };
    } finally {
      clearTimeout(timer);
    }
  }

  // waitForRestart: start
  // waitForRestart says what the page waits for, then loads the Box screen
  // again once ESM answers with version to (or without the login: it then
  // ends on the login page). The address has no fragment, which a browser
  // would take as a jump within the page, without loading anything, when it
  // is the address the page is at (/box, from the menu); and no query, so
  // no old notice shows and the health check (?check=1) is not run again.
  // After WAIT_FOR it says that ESM did not come back. It stops when the
  // page goes away.
  async function waitForRestart(to) {
    say('ESM ' + to + ' is in place; waiting for ESM to restart...');
    var w = awaitEsm(to, {
      now: function () { return Date.now(); },
      later: function (fn, ms) { return setTimeout(fn, ms); },
      cancel: function (h) { clearTimeout(h); },
      look: lookAtBox
    });
    window.addEventListener('pagehide', w.stop);
    var how = await w.done;
    window.removeEventListener('pagehide', w.stop);
    if (how === 'back') {
      location.replace('/box');
    } else if (how === 'late') {
      say('ESM ' + to + ' is in place, but ESM did not come back within a minute. If its service manager does not start it, ' +
        'start ESM again on the box (esm serve); then reload this page.', true);
    }
  }
  // waitForRestart: end

  // jobs: start
  // A long call (an update, a backup, a restore, installing a build) is
  // started as a job, with ?async=true, and followed: the page looks at
  // GET /api/v1/jobs/{id} every JOB_EVERY ms and says which step the job is
  // in, until it is not running any more. The call that starts it answers at
  // once, so nothing waits on one connection, and a page that is left does not
  // stop the job.
  var JOB_EVERY = 1000;
  // JOB_FAILS is how many looks in a row may fail before the page gives up
  // (a restart of ESM, a proxy that is not answering): a job ESM does not
  // know (404) is given up at once.
  var JOB_FAILS = 5;

  var STEP_WORDS = {
    backup: 'backing up the world',
    stop: 'stopping the server',
    switch_build: 'switching to the new build',
    start: 'starting the server',
    wait_ready: 'waiting until the server is ready',
    rollback: 'putting the old build back',
    wait_save: 'waiting for the next world save',
    copy: 'copying the save files',
    prune: 'removing the oldest backups',
    backup_current: 'keeping the current world as a backup',
    put_in_place: 'putting the backup in place',
    download: 'downloading',
    unpack: 'unpacking',
    verify: 'checking every file',
    place: 'putting the build in place'
  };

  function stepWords(name) {
    return Object.prototype.hasOwnProperty.call(STEP_WORDS, name) ? STEP_WORDS[name] : String(name).replace(/_/g, ' ');
  }

  function sizeText(n) {
    if (n >= 1073741824) { return (n / 1073741824).toFixed(1) + ' GiB'; }
    if (n >= 1048576) { return (n / 1048576).toFixed(1) + ' MiB'; }
    if (n >= 1024) { return (n / 1024).toFixed(1) + ' KiB'; }
    return n + ' bytes';
  }

  // stepProgress says how far the step that runs has come, when it says.
  function stepProgress(job) {
    var s = job.steps && job.steps.length ? job.steps[job.steps.length - 1] : null;
    if (!s) { return ''; }
    var pct = typeof s.percent === 'number' ? Math.floor(s.percent) + '%' : '';
    if (s.total > 0 && pct) { return ' (' + pct + ', ' + sizeText(s.bytes) + ' of ' + sizeText(s.total) + ')'; }
    if (pct) { return ' (' + pct + ')'; }
    if (s.bytes > 0) { return ' (' + sizeText(s.bytes) + ')'; }
    return '';
  }

  // jobText is the notice for a job that runs: what it does (label), and the
  // step it is in.
  function jobText(job, label) {
    if (!job.step) { return label + '...'; }
    return label + ': ' + stepWords(job.step) + stepProgress(job) + '...';
  }

  // followJob looks at job with io.get(id) (which resolves the job, or rejects
  // with an error that may have a status) every JOB_EVERY ms until it is not
  // running, and tells io.show(job) each time the notice for it changes, and
  // the job that ended last of all. io brings the timers (later(fn, ms),
  // cancel(h)) so that a test can run it without a page. It gives {done,
  // stop}: done resolves with the job that ended, or rejects with the error
  // of the look that gave up; stop() ends the following (the page goes away).
  function followJob(job, io) {
    var timer = null;
    var over = false;
    var fails = 0;
    var shown = null;
    var finish = null;
    var fail = null;
    var done = new Promise(function (resolve, reject) { finish = resolve; fail = reject; });
    function show(j) {
      var t = io.text(j);
      if (t !== shown) {
        shown = t;
        io.show(j, t);
      }
    }
    function look() {
      io.get(job.id).then(function (j) {
        if (over) { return; }
        fails = 0;
        if (j.state !== 'running') {
          over = true;
          finish(j);
          return;
        }
        show(j);
        timer = io.later(look, JOB_EVERY);
      }, function (e) {
        if (over) { return; }
        fails++;
        if ((e && e.status === 404) || fails >= JOB_FAILS) {
          over = true;
          fail(e);
          return;
        }
        timer = io.later(look, JOB_EVERY);
      });
    }
    show(job);
    timer = io.later(look, JOB_EVERY);
    return { done: done, stop: function () { over = true; io.cancel(timer); } };
  }
  // jobs: end

  // The job that this page follows, for the button that cancels it.
  var following = '';

  function jobCancelButtons(on) {
    each('[data-job-cancel]', function (b) { b.hidden = !on; });
  }

  // runJob starts the call at path as a job, says the step it is in until it
  // ends, and resolves with the job when it is done. A job that failed
  // rejects with the error of the call (its code too); one that was cancelled
  // rejects with the code 'cancelled'.
  async function runJob(path, body, label) {
    var job = await call('POST', path + '?async=true', body);
    following = job.id;
    var follow = followJob(job, {
      text: function (j) { return jobText(j, label); },
      show: function (j, text) {
        say(text);
        jobCancelButtons(j.cancellable);
      },
      get: function (id) { return call('GET', '/api/v1/jobs/' + enc(id)); },
      later: function (fn, ms) { return setTimeout(fn, ms); },
      cancel: function (h) { clearTimeout(h); }
    });
    window.addEventListener('pagehide', follow.stop);
    try {
      job = await follow.done;
    } catch (e) {
      throw new Error(label + ': the page lost track of the job (' + e.message + '). It may still be running; load the page again to see.');
    } finally {
      window.removeEventListener('pagehide', follow.stop);
      following = '';
      jobCancelButtons(false);
    }
    if (job.state === 'failed') {
      var err = new Error(job.error && job.error.message ? job.error.message : 'The job failed.');
      err.code = job.error ? job.error.code : '';
      throw err;
    }
    if (job.state === 'cancelled') {
      var stop = new Error(label + ' was cancelled.');
      stop.code = 'cancelled';
      throw stop;
    }
    return job;
  }

  // navigate: start
  // withoutFragment is the address u with no fragment.
  function withoutFragment(u) {
    var c = new URL(u.href);
    c.hash = '';
    return c.href;
  }

  // go loads the page at path (a path with its query and fragment) as a new
  // document, also when path is the address the page is at but for the
  // fragment: location.replace(path) is then only a jump to the fragment
  // within the page, which a browser takes without loading anything, so a
  // change or a check that gave the same notice as before seemed to do
  // nothing. The address is then set to path, fragment and all, and loaded
  // again: the page still ends at the section the fragment names.
  function go(path) {
    var cur = new URL(location.href);
    var to = new URL(path, cur);
    if (path.indexOf('#') < 0 || withoutFragment(cur) !== withoutFragment(to)) {
      location.replace(path);
      return;
    }
    history.replaceState(history.state, '', to.pathname + to.search + to.hash);
    location.reload();
  }

  // done loads the page again with a fixed notice the server shows.
  function done(code) {
    var u = new URL(location.href);
    u.searchParams.set('done', code);
    go(u.pathname + u.search + u.hash);
  }

  // releaseCheckPath is the Box screen at its release section with the
  // notice for what a check for a newer ESM release found: a wait the
  // release server asks for, the result of a look less than a minute old,
  // or a look made now.
  function releaseCheckPath(r) {
    var code = r.retry_after ? 'release-wait' : (r.reused ? 'release-recent' : 'release');
    return '/box?done=' + code + '#esm-h';
  }
  // navigate: end

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
      await runJob(inst(b.dataset.name) + '/backup', null, 'Backing up ' + b.dataset.name);
      done('backup');
    },
    restore: async function (b) {
      say('Restoring. The server stops and starts again; this can take a while.');
      await runJob(inst(b.dataset.name) + '/restore', { backup: b.dataset.id }, 'Restoring ' + b.dataset.name);
      done('restored');
    },
    // cancel-job stops the install that this page follows (only an install
    // can be cancelled, and not once it puts its build in place); the page
    // that follows it says that it was cancelled.
    'cancel-job': async function () {
      if (!following) { return; }
      say('Cancelling the install...');
      await call('DELETE', '/api/v1/jobs/' + enc(following));
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
    // release-check looks for a newer ESM release now (or gets the result
    // of a look that ended less than a minute ago, or of the release
    // server's request to wait), then shows the Box screen again with it,
    // like done() but without ?check=1, so that a health check on the
    // screen is not run again unasked. A second check that gives the same
    // notice goes to the address the page is at: go loads it again.
    'release-check': async function () {
      say('Looking for a newer ESM release...');
      var r = await call('POST', '/api/v1/box/release-check');
      go(releaseCheckPath(r));
    },
    'self-update': async function () {
      var r;
      try {
        r = await call('POST', '/api/v1/box/self-update');
      } catch (e) {
        // Nothing would start ESM again: only on a second, explicit yes.
        if (e.code !== 'no_service_manager' ||
            !window.confirm(e.message + '\n\nUpdate and end ESM anyway? It then has to be started again by hand on the box.')) {
          throw e;
        }
        r = await call('POST', '/api/v1/box/self-update', { exit_anyway: true });
      }
      if (r.restarting) {
        // The button stays busy while the page waits for the new ESM.
        await waitForRestart(r.to);
      } else {
        say('ESM ' + r.to + ' is in place and ESM ends now. Start it again on the box (esm serve), then reload this page.');
      }
    },
    revoke: async function (b) {
      await call('DELETE', '/api/v1/tokens/' + enc(b.dataset.id));
      done('revoked');
    },
    // reload-settings is the way out of the notice of a save that was
    // refused because the settings changed meanwhile: the page is loaded
    // anew, without the notice of an earlier save, and not as a reload,
    // which a browser may answer with the values typed into the form (the
    // form has autocomplete off as well).
    'reload-settings': async function () {
      var u = new URL(location.href);
      u.searchParams.delete('done');
      go(u.pathname + u.search);
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
      await runJob(inst(f.dataset.name) + '/update', { to: to }, 'Updating ' + f.dataset.name + ' to build ' + to);
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
      try {
        await runJob('/api/v1/builds', body, 'Installing ' + body.source);
      } catch (e) {
        // A cancel is no failure: nothing was installed, and it says so.
        if (e.code !== 'cancelled') { throw e; }
        say('The install was cancelled. Nothing was installed.');
        return;
      }
      done('installed');
    },
    // password changes the login's own password: the current one is
    // checked by the API (a wrong one counts as a failed login), and the
    // login's other sessions end.
    password: async function (f) {
      if (val(f, 'password') !== val(f, 'password2')) { throw new Error('The two new passwords are not the same.'); }
      await call('POST', '/api/v1/session/password', { current_password: val(f, 'current'), new_password: val(f, 'password') });
      f.reset();
      done('password');
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

  // saveRules is the rules map a save sends (the API replaces the whole
  // map). fields are the form's rule fields as {key, value, game, dropped}.
  // A game setting left empty is not set and left out, so the server uses
  // its default; any other rule goes as it is unless it is removed. It
  // uses nothing of the page, so the page tests run it alone.
  // saveRules: start
  function saveRules(fields) {
    var rules = {};
    fields.forEach(function (x) {
      if (x.game ? x.value !== '' : !x.dropped) { rules[x.key] = x.value; }
    });
    return rules;
  }
  // saveRules: end

  // settingsSave: start
  // ifMatch is the extra headers of a change made on what a form showed at
  // revision rev (a number, or its text from the page): If-Match with the
  // revision as a strong entity tag, as the API sends it in ETag. Without a
  // revision there is nothing to name, and no header.
  function ifMatch(rev) {
    var n = parseInt(rev, 10);
    return n >= 1 ? { 'If-Match': '"' + n + '"' } : undefined;
  }

  // settingsSave makes the changes of a settings form, in order: the
  // settings (PATCH base, when parts.change has any) and then the passwords
  // and tokens (PUT base/secrets, when parts.secrets has any), where base
  // is the server's path in the API. Each is made only if the server's
  // settings are still at the revision the form showed (If-Match): the
  // second on the revision that the answer to the first gave, so the form's
  // own save does not refuse itself, but a change by anyone else, an
  // admin on the page or the command line, a website, another tab, does.
  // io.call(method, path, body, headers) is the page's call: it resolves
  // with the decoded answer, and rejects with an Error that has status and
  // code. settingsSave resolves {changed, saved}: changed says that the
  // API refused with 412 changed, so that nothing, or not all, was saved,
  // and saved lists what was saved before that ('settings'). Any other
  // failure is thrown as it is.
  async function settingsSave(base, revision, parts, io) {
    var saved = [];
    var rev = revision;
    try {
      if (Object.keys(parts.change).length) {
        var r = await io.call('PATCH', base, parts.change, ifMatch(rev));
        rev = r && r.revision;
        saved.push('settings');
      }
      if (Object.keys(parts.secrets).length) {
        await io.call('PUT', base + '/secrets', parts.secrets, ifMatch(rev));
        saved.push('passwords');
      }
    } catch (e) {
      if (e && e.status === 412 && e.code === 'changed') { return { changed: true, saved: saved }; }
      throw e;
    }
    return { changed: false, saved: saved };
  }

  // changedNotice shows, in box, the notice of a save that settingsSave
  // said was refused because the settings changed: the paragraph for
  // nothing saved (data-changed="all") or, when the settings were saved
  // and the passwords were not, the one for that (data-changed="part").
  // It returns the notice's button, for the page to focus.
  function changedNotice(box, saved) {
    var which = saved.length ? 'part' : 'all';
    Array.prototype.forEach.call(box.querySelectorAll('[data-changed]'), function (p) {
      p.hidden = p.getAttribute('data-changed') !== which;
    });
    box.hidden = false;
    return box.querySelector('button');
  }
  // settingsSave: end

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
        if (!/^[A-Za-z][A-Za-z0-9]{0,63}$/.test(key)) {
          say('A rule name starts with a letter and has only letters and digits.', true);
          return;
        }
        var known = f.elements['rule:' + key];
        if (known) {
          say(known.hasAttribute('data-game') ? key + ' has a field of its own above; set it there.' : 'Rule ' + key + ' is set already; change its value in the list.', true);
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
      var ruleFields = [];
      var rulesChanged = false;
      fields().forEach(function (el) {
        var n = el.name;
        if (n.indexOf('rule:') === 0) {
          var key = n.slice(5);
          var drop = f.elements['drop:' + key];
          if (changed(el) || el.closest('[data-added]')) { rulesChanged = true; }
          ruleFields.push({ key: key, value: el.value, game: el.hasAttribute('data-game'), dropped: !!(drop && drop.checked) });
          return;
        }
        if (n.indexOf('drop:') === 0) {
          if (changed(el)) { rulesChanged = true; }
          return;
        }
        if (!changed(el)) { return; }
        // Passwords and tokens: a new value, or cleared.
        if (el.hasAttribute('data-secret')) {
          if (el.value !== '') { secrets[n] = el.value; }
          return;
        }
        if (n.indexOf('clear_') === 0) {
          if (el.checked) { secrets[n.slice(6)] = ''; }
          return;
        }
        switch (n) {
          case 'max_players': b.max_players = parseInt(el.value, 10) || 0; break;
          case 'listed': b.listed = el.checked; break;
          default: b[n] = el.value;
        }
      });
      if (rulesChanged) { b.rules = saveRules(ruleFields); }
      return { change: b, secrets: secrets };
    }

    async function save(restart) {
      var name = f.dataset.name;
      var res = await settingsSave(inst(name), f.dataset.revision, body(), { call: call });
      if (res.changed) {
        var box = document.getElementById('changed-meanwhile');
        if (!box) { throw new Error('The settings changed since this page was loaded. Reload the page.'); }
        var reload = changedNotice(box, res.saved);
        box.scrollIntoView({ block: 'center' });
        if (reload) { reload.focus(); }
        return;
      }
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
  // little later each time; when the login is gone (sessionEnded: a 401,
  // or the 429 of an address limited for failed authentications) it goes
  // to the login instead.
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
          if (sessionEnded('/api/v1/session', r.status)) { toLogin(); return; }
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
