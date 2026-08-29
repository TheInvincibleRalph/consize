/* app.js — hash-routed views: Dashboard, Recommendations, Workloads,
 * Workload detail, Audit.
 *
 * No framework, no build. All DOM is built with el() and textContent,
 * so API data is never injected as HTML (XSS-safe by construction).
 *
 * Field names come straight from the Go store models (PascalCase):
 *   Workload:        ID, Name, Namespace, Kind, Source ("k8s"|"db"),
 *                    RequestMemBytes, RequestCPUMilli, DBClass, DBReplicas,
 *                    DBMaintenanceWindow, DBProvider, ...
 *   Recommendation:  ID, WorkloadID, WorkloadName, Namespace,
 *                    Resource ("cpu"|"memory"|"class"), CurrentValue,
 *                    ProposedValue, SavingsMonthly, Confidence, Status,
 *                    ClassCurrent, ClassProposed, CreatedAt
 *   ApplyEvent:      ID, RecommendationID, WorkloadID, Actor, Mode, Result,
 *                    StepNumber, TotalSteps, Diff, CreatedAt
 *   VerificationRun: ID, ApplyEventID, Verdict, SLIs, BaselineStart/End,
 *                    PostStart/End, CreatedAt
 * Units: memory values are bytes, CPU values are milli-cores.
 *
 * The backend is adding three fields in parallel: Recommendation.Risk /
 * RiskReasons, extended /savings (realized savings + per-owner
 * breakdown), and /workloads/{id}/series. Everything touching those is
 * feature-checked: absent fields degrade silently, never crash.
 */
(function () {
  'use strict';

  var api = window.ConsizeAPI;
  var MiB = 1024 * 1024;

  /* ---------- tiny DOM helper ---------- */

  function el(tag, attrs, children) {
    attrs = attrs || {};
    var node = document.createElement(tag);
    Object.keys(attrs).forEach(function (k) {
      var v = attrs[k];
      if (v === undefined || v === null || v === false) return;
      if (k === 'class') node.className = v;
      else if (k === 'onclick') node.addEventListener('click', v);
      else if (v === true) node.setAttribute(k, '');
      else node.setAttribute(k, v);
    });
    // Accepts el('div', attrs, c1, c2, ...) and el('div', attrs, [c1, c2]).
    var kids = Array.prototype.slice.call(arguments, 2).filter(function (k) {
      return k !== undefined && k !== null; // null children = conditional slots
    });
    if (kids.length === 1 && Array.isArray(kids[0])) kids = kids[0];
    kids.forEach(function (c) {
      node.appendChild(c instanceof Node ? c : document.createTextNode(String(c)));
    });
    return node;
  }

  function th(label, cls) { return el('th', { scope: 'col', class: cls }, label); }
  function td(cls, children) { return el('td', cls ? { class: cls } : {}, children); }
  function theadRow(labels) {
    return el('thead', {}, el('tr', {}, labels.map(function (l) {
      return th(l.label, l.right ? 'num' : undefined);
    })));
  }

  // First defined value among candidate keys — defensive feature-checking
  // for fields the backend is adding in parallel.
  function pick(obj, keys) {
    if (!obj || typeof obj !== 'object') return undefined;
    for (var i = 0; i < keys.length; i++) {
      if (obj[keys[i]] !== undefined && obj[keys[i]] !== null) return obj[keys[i]];
    }
    return undefined;
  }

  /* ---------- formatting ---------- */

  function money(n) {
    var v = Number(n);
    if (isNaN(v)) return '—';
    return '$' + v.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }
  function pct(n) {
    var v = Number(n);
    return isNaN(v) ? '—' : Math.round(v * 100) + '%';
  }

  function fmtBytes(n) {
    var m = n / MiB;
    if (m >= 1024) return (m / 1024).toFixed(2) + ' GiB';
    return (m >= 100 ? m.toFixed(0) : m.toFixed(1)) + ' MiB';
  }
  function fmtMilli(n) { return n >= 1000 ? (n / 1000).toFixed(2) + ' cores' : n + 'm'; }

  // memory recommendations are stored as bytes, cpu as milli-cores.
  function currentOf(r) { return r.Resource === 'memory' ? fmtBytes(r.CurrentValue) : fmtMilli(r.CurrentValue); }
  function proposedOf(r) { return r.Resource === 'memory' ? fmtBytes(r.ProposedValue) : fmtMilli(r.ProposedValue); }

  function fmtTime(iso) {
    if (!iso) return '—';
    var d = new Date(iso);
    return isNaN(d.getTime()) ? String(iso) : d.toLocaleString();
  }

  // Compact number for summaries (80, 0.12, 5 — no float noise).
  function fmtNum(v) {
    if (typeof v !== 'number') return String(v);
    return Number.isInteger(v) ? String(v) : String(Math.round(v * 100) / 100);
  }

  // SLIs is a JSON object of signal -> value. Values may be scalars or
  // evidence maps (the verifier fills threshold, post_above_threshold,
  // longest_breach_minutes, ... for cpu/mem AND db class signals) —
  // render both generically: "cpu_saturation={threshold=80, breach=0}".
  function sliSummary(slis) {
    if (!slis || typeof slis !== 'object') return '—';
    var parts = Object.keys(slis).map(function (k) {
      var v = slis[k];
      if (v && typeof v === 'object') {
        var inner = Object.keys(v).map(function (ik) { return ik + '=' + fmtNum(v[ik]); }).join(', ');
        return k + '={' + inner + '}';
      }
      return k + '=' + fmtNum(v);
    });
    return parts.length ? parts.join(' · ') : '—';
  }

  // "db.t3.xlarge" -> "xlarge" — the audit trail and step plans use the
  // short family form, the recommendations table the full class name.
  function shortClass(name) {
    return name ? String(name).split('.').pop() : name;
  }

  // One recommendation's before/after cell. class rows carry
  // ClassCurrent/ClassProposed (CurrentValue/ProposedValue are 0 and
  // must not be rendered as "0m"/"0 MiB").
  function recChange(r) {
    if (r.Resource === 'class') {
      return el('span', { class: 'delta' },
        el('span', { class: 'muted' }, 'class '),
        el('code', {}, r.ClassCurrent || '—'),
        el('span', { class: 'arrow' }, '→'),
        el('code', {}, r.ClassProposed || '—'));
    }
    return el('span', { class: 'delta' },
      el('code', {}, currentOf(r)), el('span', { class: 'arrow' }, '→'), el('code', {}, proposedOf(r)));
  }

  // One apply-event Diff (wire keys are snake_case; class events name
  // the change via current_class/proposed_class, request/limit are 0).
  function diffLine(d) {
    if (d.resource === 'class') {
      return shortClass(d.current_class) + ' → ' + shortClass(d.proposed_class);
    }
    if (d.resource === 'memory') return fmtBytes(d.current_request) + ' → ' + fmtBytes(d.proposed_request);
    if (d.resource === 'cpu') return fmtMilli(d.current_request) + ' → ' + fmtMilli(d.proposed_request);
    return '—';
  }

  function diffCell(d) {
    var from, to;
    if (!d || !d.resource) return el('span', { class: 'muted' }, '—');
    if (d.resource === 'class') { from = shortClass(d.current_class); to = shortClass(d.proposed_class); }
    else if (d.resource === 'memory') { from = fmtBytes(d.current_request); to = fmtBytes(d.proposed_request); }
    else if (d.resource === 'cpu') { from = fmtMilli(d.current_request); to = fmtMilli(d.proposed_request); }
    else return el('span', { class: 'muted' }, '—');
    return el('span', { class: 'delta mono' },
      el('code', {}, from), el('span', { class: 'arrow' }, '→'), el('code', {}, to));
  }

  /* ---------- avatars & badges ---------- */

  function avatarEl(name, kind, large) {
    var initial = String(name || '?').charAt(0).toUpperCase();
    return el('span', { class: 'avatar ' + kind + (large ? ' lg' : '') }, initial);
  }

  function workloadCell(name, namespace, id, kind) {
    return el('div', { class: 'wl-cell' },
      avatarEl(name, kind),
      el('a', { href: '#/workloads/' + id },
        el('div', { class: 'wl' }, el('strong', {}, name), el('span', {}, namespace))));
  }

  var STATUS_COLOR = {
    pending: 'blue', applied: 'green', verified: 'teal',
    rolled_back: 'red', superseded: 'gray', rejected: 'red'
  };
  var RESULT_COLOR = { planned: 'gray', applied: 'green', reverted: 'red' };
  var VERDICT_COLOR = { passed: 'green', failed: 'red', inconclusive: 'amber' };
  var RISK_COLOR = { low: 'green', medium: 'amber', high: 'red' };

  function badge(colorMap, text) {
    return el('span', { class: 'badge ' + (colorMap[text] || 'gray') }, text);
  }
  function statusBadge(s) { return badge(STATUS_COLOR, s); }
  function resultBadge(r) { return badge(RESULT_COLOR, r); }
  function verdictBadge(v) { return badge(VERDICT_COLOR, v); }

  // Risk pill: low|medium|high from the new Recommendation.Risk field;
  // RiskReasons rides the native title tooltip. The backend marshals the
  // field as snake_case JSON ("risk"/"risk_reasons") — accept both cases.
  function riskOf(r) { return pick(r, ['Risk', 'risk']); }
  function riskReasonsOf(r) { return pick(r, ['RiskReasons', 'risk_reasons']); }

  function riskBadge(risk, reasons) {
    var text = risk == null || risk === '' ? 'n/a' : String(risk);
    var b = badge(RISK_COLOR, text);
    if (reasons) {
      b.title = Array.isArray(reasons) ? reasons.join('\n') : String(reasons);
    }
    return b;
  }

  // Kind pill: compute (cpu/memory) vs database (class) recommendations,
  // and k8s vs db workloads — one surface for both.
  function kindBadge(isDB) {
    return badge({ db: 'teal', k8s: 'blue' }, isDB ? 'database' : 'compute');
  }

  /* ---------- states ---------- */

  function loadingState(msg) {
    return el('div', { class: 'state' },
      el('div', { class: 'spinner' }),
      el('p', { class: 'state-sub' }, msg));
  }

  function emptyState(msg) {
    return el('div', { class: 'state' },
      el('p', { class: 'state-title' }, msg));
  }

  function errorState(msg) {
    return el('div', { class: 'state' },
      el('p', { class: 'state-title' }, 'API unreachable — start the API and reload'),
      el('p', { class: 'state-sub' }, msg + ' (tried ' + api.base() + ')'),
      el('button', { class: 'btn', onclick: function () { location.reload(); } }, 'Reload'));
  }

  // A 404 is not an API failure — no "API unreachable" framing.
  function notFoundState(title, sub, href, linkLabel) {
    return el('div', { class: 'state' },
      el('p', { class: 'state-title' }, title),
      el('p', { class: 'state-sub' }, sub),
      el('a', { class: 'btn', href: href }, linkLabel));
  }

  function pageHead(title, sub) {
    return el('div', { class: 'page-head' },
      el('h1', {}, title),
      el('p', { class: 'sub' }, sub));
  }

  function card(title, body, sub) {
    return el('div', { class: 'card' },
      el('div', { class: 'card-head' }, el('h2', { class: 'card-title' }, title),
        sub ? el('span', { class: 'card-sub' }, sub) : null),
      body);
  }

  /* ---------- API status pill ---------- */

  function updateApiStatus() {
    var pill = document.getElementById('api-status');
    api.healthz().then(function () {
      pill.className = 'api-status ok';
      pill.replaceChildren(el('span', { class: 'dot' }), el('span', { class: 'api-status-text' }, 'API connected'));
    }, function () {
      pill.className = 'api-status down';
      pill.replaceChildren(el('span', { class: 'dot' }), el('span', { class: 'api-status-text' }, 'API unreachable'));
    });
  }

  /* ---------- dashboard ---------- */

  function stat(label, value, sub, accent) {
    return el('div', { class: 'stat' },
      el('div', { class: 'label' }, label),
      el('div', { class: 'value' + (accent ? ' accent' : '') }, value),
      el('div', { class: 'sub' }, sub));
  }

  function safetyStrip() {
    var steps = [
      ['Analyze', '14 days of P95 usage become compute and database recommendations.'],
      ['Guarded apply', 'Dry-run first, maintenance windows, approval policy.'],
      ['Verify', 'SLI evidence measured against the pre-change baseline.'],
      ['Auto-rollback', 'A failed verification reverts the change automatically.'],
      ['Audit', 'INSERT-only trail: every event, diff, and verdict, forever.']
    ];
    var lis = steps.map(function (s, i) {
      return el('li', {}, el('div', { class: 'safety-step' },
        el('span', { class: 'safety-num' }, String(i + 1)),
        el('div', {}, el('strong', {}, s[0]), el('p', {}, s[1]))));
    });
    return el('div', { class: 'safety' },
      el('div', { class: 'safety-kicker' }, 'Safety engine'),
      el('h2', {}, 'Every change is analyzed, guarded, verified — and reversible. ',
        el('span', { class: 'loop' }, 'Analyze → Guarded apply → Verify → Auto-rollback → Audit')),
      el('ul', { class: 'safety-steps' }, lis));
  }

  function panel(title, moreHref, body) {
    var head = el('div', { class: 'panel-head' }, el('h2', { class: 'panel-title' }, title));
    if (moreHref) head.appendChild(el('a', { class: 'panel-more', href: moreHref }, 'view all →'));
    return el('div', { class: 'panel' },
      head,
      el('div', { class: 'panel-body' }, body));
  }

  function recentAppliesTable(applies, nameOf) {
    if (!applies.length) return emptyState('No apply events yet.');
    return el('div', { class: 'tbl-wrap' },
      el('table', {},
        theadRow([{ label: 'Event' }, { label: 'Workload' }, { label: 'Result' }, { label: 'Step' }, { label: 'Actor' }, { label: 'Created' }]),
        el('tbody', {}, applies.map(function (e) {
          return el('tr', {},
            td('mono muted', '#' + e.ID),
            td(undefined, nameOf(e.WorkloadID)),
            td(undefined, resultBadge(e.Result)),
            td('mono muted', e.StepNumber + '/' + e.TotalSteps),
            td('muted', e.Actor),
            td('muted', fmtTime(e.CreatedAt)));
        }))));
  }

  function recentRunsTable(runs) {
    if (!runs.length) return emptyState('No verification runs yet.');
    return el('div', { class: 'tbl-wrap' },
      el('table', {},
        theadRow([{ label: 'Run' }, { label: 'Apply' }, { label: 'Verdict' }, { label: 'SLIs' }, { label: 'Created' }]),
        el('tbody', {}, runs.map(function (r) {
          return el('tr', {},
            td('mono muted', '#' + r.ID),
            td('mono muted', '#' + r.ApplyEventID),
            td(undefined, verdictBadge(r.Verdict)),
            td(undefined, el('span', { class: 'sli-line', title: sliSummary(r.SLIs) },
              el('span', { class: 'trunc mono muted' }, sliSummary(r.SLIs)))),
            td('muted', fmtTime(r.CreatedAt)));
        }))));
  }

  // by_owner arrives either as an array of owner rows, or as an object
  // keyed by owner ({ owner: { projected_monthly, realized_monthly } }) —
  // normalize both to [{ owner, projected, realized }]-style rows.
  function byOwnerRows(raw) {
    if (Array.isArray(raw)) return raw;
    if (raw && typeof raw === 'object') {
      return Object.keys(raw).map(function (owner) {
        var v = raw[owner];
        if (v && typeof v === 'object') return Object.assign({ owner: owner }, v);
        return { owner: owner, value: v }; // scalar shorthand
      });
    }
    return [];
  }

  // Per-owner savings breakdown — only rendered when the /savings body
  // carries by_owner (a parallel backend effort). Owner and amount field
  // names are picked defensively.
  function byOwnerCard(rows) {
    var realizedKeys = ['realized_monthly_savings', 'realized_monthly', 'realized_savings_monthly', 'realized'];
    var hasRealized = rows.some(function (r) { return pick(r, realizedKeys) != null; });
    var head = [{ label: 'Owner' }, { label: 'Projected / mo', right: true }];
    if (hasRealized) head.push({ label: 'Realized / mo', right: true });
    var tbody = rows.map(function (r) {
      var owner = pick(r, ['owner', 'name', 'actor', 'owner_name', 'ownerName']) || 'unknown';
      var proj = pick(r, ['projected_monthly_savings', 'projected_monthly', 'projected_savings_monthly', 'projected', 'savings', 'amount']);
      var real = pick(r, realizedKeys);
      return el('tr', {},
        td(undefined, el('span', { class: 'mono' }, owner)),
        td('num mono', proj == null ? '—' : money(proj)),
        hasRealized ? td('num mono', real == null ? '—' : money(real)) : null);
    });
    return el('div', { class: 'card' },
      el('div', { class: 'card-head' }, el('h2', { class: 'card-title' }, 'Savings by owner')),
      el('div', { class: 'tbl-wrap' },
        el('table', {}, theadRow(head), el('tbody', {}, tbody))));
  }

  function renderDashboard(root) {
    root.replaceChildren(loadingState('Loading dashboard…'));
    return Promise.all([api.savings(), api.workloads(), api.applies(), api.verificationRuns()])
      .then(function (results) {
        var sav = results[0] || {};
        var workloads = results[1] || [];
        var applies = results[2] || [];
        var runs = results[3] || [];

        var projected = pick(sav, ['projected_monthly_savings', 'projected_monthly', 'projected']);
        var realized = pick(sav, ['realized_monthly_savings', 'realized_monthly', 'realized_savings_monthly']);
        var active = pick(sav, ['active_recommendations', 'pending_recommendations', 'pending_count']);
        var byOwner = pick(sav, ['by_owner', 'byOwner', 'owners']);

        var names = new Map(workloads.map(function (w) { return [w.ID, w.Namespace + '/' + w.Name]; }));
        var nameOf = function (id) { return names.get(id) || 'workload #' + id; };

        var kids = [
          pageHead('Dashboard', 'Rightsizing with a safety engine — every recommendation is applied, verified, and audited.'),
          el('div', { class: 'stats' },
            stat('Workloads under analysis', String(workloads.length), 'compute + databases under management'),
            stat('Pending recommendations', active == null ? '—' : String(active), 'waiting on a decision'),
            stat('Projected monthly savings', money(projected), 'sum of pending recommendations', true),
            stat('Realized monthly savings', realized == null ? '—' : money(realized),
              realized == null ? 'verified applies not yet reported' : 'from verified applies', realized != null)),
          safetyStrip()
        ];
        // replaceChildren stringifies non-Node args ("null"), so build
        // the conditional by-owner card into the list instead of a slot.
        var ownerRows = byOwnerRows(byOwner);
        if (ownerRows.length) kids.push(byOwnerCard(ownerRows));
        kids.push(el('div', { class: 'panels' },
          panel('Recent apply events', '#/audit', recentAppliesTable(applies.slice(0, 8), nameOf)),
          panel('Recent verification runs', '#/audit', recentRunsTable(runs.slice(0, 8)))));
        root.replaceChildren.apply(root, kids);
      }, function (err) {
        root.replaceChildren(errorState('Dashboard data failed to load.'));
      });
  }

  /* ---------- workloads ---------- */

  function renderWorkloads(root) {
    root.replaceChildren(loadingState('Loading workloads…'));
    return api.workloads().then(function (workloads) {
      var state = { filter: 'all', list: workloads };
      var segs = ['all', 'compute', 'database'].map(function (f) {
        var b = el('button', { class: 'seg' + (f === state.filter ? ' active' : '') },
          f === 'all' ? 'All surfaces' : f.charAt(0).toUpperCase() + f.slice(1));
        b.addEventListener('click', function () {
          state.filter = f;
          segs.forEach(function (x) { x.classList.toggle('active', x === b); });
          paint();
        });
        return b;
      });

      var body = el('div', { class: 'panel-body' });
      var container = el('div', { class: 'card' },
        el('div', { class: 'card-head' },
          el('h2', { class: 'card-title' }, 'Workloads'),
          el('span', { class: 'card-sub' }, 'compute + databases, one surface')),
        el('div', { class: 'filter-row' }, el('span', { class: 'seg-row' }, segs)),
        body);

      function isDB(w) { return w.Source === 'db' || w.Kind === 'database'; }
      function paint() {
        var rows = state.list.filter(function (w) {
          return state.filter === 'all' || (state.filter === 'database') === isDB(w);
        });
        var hasRisk = state.list.some(function (w) { return riskOf(w) != null; });
        if (!state.list.length) {
          body.replaceChildren(emptyState('No workloads yet — run the collector.'));
          return;
        }
        if (!rows.length) {
          body.replaceChildren(emptyState('No workloads match this surface.'));
          return;
        }
        body.replaceChildren(el('div', { class: 'tbl-wrap' },
          el('table', {},
            theadRow([{ label: 'Workload' }, { label: 'Kind' }, { label: 'Size' },
              { label: 'Replicas' }, { label: 'Maintenance window' }, { label: 'Source' }]
              .concat(hasRisk ? [{ label: 'Risk' }] : [])),
            el('tbody', {}, rows.map(function (w) {
              var db = isDB(w);
              var size = db
                ? el('span', { class: 'delta' },
                    el('code', {}, w.DBClass || '—'),
                    w.DBProvider ? el('span', { class: 'chip prov' }, w.DBProvider) : null)
                : el('span', { class: 'mono muted' }, fmtMilli(w.RequestCPUMilli), ' / ', fmtBytes(w.RequestMemBytes));
              return el('tr', {},
                td(undefined, workloadCell(w.Name, w.Namespace, w.ID, db ? 'db' : 'k8s')),
                td(undefined, kindBadge(db)),
                td(undefined, size),
                td('mono muted', db ? String(w.DBReplicas) : '—'),
                td('mono muted', db && w.DBMaintenanceWindow ? w.DBMaintenanceWindow : '—'),
                td(undefined, el('span', { class: 'chip ' + (db ? 'db' : 'k8s') }, db ? 'db' : 'k8s')),
                hasRisk ? td(undefined, riskOf(w) != null ? riskBadge(riskOf(w), riskReasonsOf(w)) : el('span', { class: 'muted' }, '—')) : null);
            })))));
      }

      paint();
      root.replaceChildren(container);
    }, function (err) {
      root.replaceChildren(errorState('Workloads failed to load.'));
    });
  }

  /* ---------- recommendations ---------- */

  // The backend paginates /recommendations (?limit= defaults to 100,
  // ?offset=, and every response carries pagination.total). We render
  // one PAGE at a time; "Load more" fetches the next offset page until
  // we hold pagination.total rows. Dedupe-by-ID keeps overlapping
  // fetches idempotent.
  var PAGE = 100;

  function renderRecommendations(root) {
    var view = VIEWS.recommendations;
    view.all = [];
    view.shown = 0;
    view.total = null;
    view.noMore = false;

    root.replaceChildren(loadingState('Loading recommendations…'));
    return loadMore(root);
  }

  function loadMore(root) {
    var view = VIEWS.recommendations;
    if (view.busy) return view.busy;
    view.busy = api.recommendations({ offset: view.all.length }).then(function (body) {
      var fresh = body.recommendations || [];
      var page = body.pagination || null;

      // Append what we don't already have (dedupe is a safety net for
      // overlapping or repeated fetches).
      var seen = new Set(view.all.map(function (r) { return r.ID; }));
      var added = 0;
      fresh.forEach(function (r) {
        if (!seen.has(r.ID)) { seen.add(r.ID); view.all.push(r); added++; }
      });

      // Sorted by monthly savings, descending (client-side).
      view.all.sort(function (a, b) { return (b.SavingsMonthly || 0) - (a.SavingsMonthly || 0); });

      if (page && typeof page.total === 'number') {
        // Live contract: one page per fetch, stop exactly when we hold
        // everything the server counts as matching.
        view.total = page.total;
        view.shown = view.all.length;
        view.noMore = view.all.length >= page.total;
      } else {
        // Fallback for a non-paginating backend: the fetch returned the
        // whole list, so reveal locally-cached rows in PAGE-sized steps.
        view.shown = Math.min(view.all.length, view.shown + PAGE);
        view.noMore = added === 0 && view.shown >= view.all.length;
      }
      paintRecommendations(root);
      view.busy = null;
    }, function (err) {
      view.busy = null;
      root.replaceChildren(errorState('Recommendations failed to load.'));
    });
    return view.busy;
  }

  function paintRecommendations(root) {
    var view = VIEWS.recommendations;
    var visible = view.all.slice(0, view.shown);

    // Risk column only exists once the backend ships Recommendation.Risk.
    var hasRisk = view.all.some(function (r) { return riskOf(r) != null; });

    var body;
    if (!view.all.length) {
      body = el('div', { class: 'panel-body' }, emptyState('No recommendations yet — run the analyzer.'));
    } else {
      var rows = visible.map(function (r) {
        var db = r.Resource === 'class';
        var risk = hasRisk ? (riskOf(r) != null ? riskBadge(riskOf(r), riskReasonsOf(r)) : el('span', { class: 'muted' }, '—')) : null;
        return el('tr', {},
          td(undefined, workloadCell(r.WorkloadName, r.Namespace, r.WorkloadID, db ? 'db' : 'k8s')),
          td(undefined, kindBadge(db)),
          td(undefined, recChange(r)),
          td('num mono', money(r.SavingsMonthly)),
          td('num mono', pct(r.Confidence)),
          risk ? td(undefined, risk) : null,
          td(undefined, statusBadge(r.Status)),
          td('muted', fmtTime(r.CreatedAt)),
          td(undefined, el('button', {
            class: 'btn small',
            onclick: function () { openApplyModal(r); }
          }, 'Apply')));
      });

      var moreBtn = el('button', {
        class: 'btn',
        disabled: view.noMore || !view.all.length,
        onclick: function () { loadMore(root); }
      }, 'Load more');
      if (view.noMore && view.all.length) {
        moreBtn.title = view.total != null
          ? 'All ' + view.total + ' recommendations are shown'
          : 'All fetched recommendations are shown';
      }

      var head = [{ label: 'Workload' }, { label: 'Kind' }, { label: 'One-step plan' },
        { label: 'Savings / mo', right: true }, { label: 'Confidence', right: true }];
      if (hasRisk) head.push({ label: 'Risk' });
      head = head.concat([{ label: 'Status' }, { label: 'Created' }, { label: 'Apply' }]);

      body = el('div', {},
        el('div', { class: 'tbl-wrap' },
          el('table', {},
            theadRow(head),
            el('tbody', {}, rows))),
        el('div', { class: 'load-row' },
          moreBtn,
          el('span', { class: 'count' },
            'Showing ' + visible.length + ' of ' +
            (view.total != null ? view.total : view.all.length) +
            ' recommendations · sorted by savings')));
    }

    root.replaceChildren(el('div', { class: 'card' },
      el('div', { class: 'card-head' },
        el('h2', { class: 'card-title' }, 'Recommendations'),
        el('span', { class: 'card-sub' }, 'ranked by monthly savings — the change cell is the one-step plan')),
      body));
  }

  /* ---------- apply modal ---------- */

  // POST /recommendations/{id}/apply. Mode policy: dry_run needs nothing,
  // approved needs an actor, auto needs the auto-db/auto label (enforced
  // server-side; blocked applies come back as reasons, 422 or Blocked).
  // The engine route picks itself from the recommendation's resource:
  // class -> DB engine (adds InWindow/Window), cpu/memory -> k8s engine.
  function openApplyModal(rec) {
    var state = { mode: 'dry_run' };

    var overlay = el('div', { class: 'modal-overlay' });
    var panel = el('div', { class: 'modal' });

    var result = el('div', { class: 'modal-result' });

    var actorInput = el('input', {
      class: 'text', type: 'text', placeholder: 'operator name, e.g. alice', disabled: true
    });
    var actorWrap = el('label', { class: 'field', hidden: true },
      'Actor (required for approved mode)', actorInput);

    var segBtns = ['dry_run', 'approved', 'auto'].map(function (m) {
      var b = el('button', { class: 'seg' + (m === state.mode ? ' active' : '') },
        m === 'dry_run' ? 'Dry run' : m);
      b.addEventListener('click', function () {
        state.mode = m;
        segBtns.forEach(function (x) { x.classList.toggle('active', x === b); });
        var approved = m === 'approved';
        actorWrap.hidden = !approved;
        actorInput.disabled = !approved;
      });
      return b;
    });

    var runBtn = el('button', { class: 'btn primary', onclick: run }, 'Run apply');
    var cancelBtn = el('button', { class: 'btn', onclick: close }, 'Close');

    function run() {
      if (runBtn.disabled) return;
      var payload = { mode: state.mode };
      if (state.mode === 'approved') {
        var actor = actorInput.value.trim();
        if (!actor) { result.replaceChildren(el('p', { class: 'err' }, 'approved mode requires an actor')); return; }
        payload.actor = actor;
      }
      runBtn.disabled = true;
      result.replaceChildren(el('p', { class: 'muted' }, 'Applying…'));
      api.apply(rec.ID, payload).then(function (body) {
        runBtn.disabled = false;
        result.replaceChildren(applyOutcome(body));
        markStale('recommendations'); // statuses may have changed
      }, function (err) {
        runBtn.disabled = false;
        result.replaceChildren(applyError(err));
      });
    }

    function close() {
      overlay.remove();
      route(); // refetch the current view so statuses update
    }

    function applyOutcome(b) {
      var parts = [];
      if (b.Blocked) {
        parts.push(blockedBox('Apply blocked', b.BlockReasons));
      } else {
        // Step plan: "step 1 of 4 — large" (short class family for class
        // applies; the remainder continues as a follow-up recommendation).
        if (b.StepNumber || b.TotalSteps) {
          var text = 'Step ' + b.StepNumber + ' of ' + b.TotalSteps;
          if (b.Diff && b.Diff.resource === 'class' && b.Diff.proposed_class) {
            text += ' — ' + shortClass(b.Diff.proposed_class);
          }
          parts.push(el('p', { class: 'plan' }, text));
        }
        if (b.Diff && b.Diff.resource) parts.push(el('p', { class: 'change' }, diffLine(b.Diff)));
        // Maintenance-window state (DB engine only).
        if (b.InWindow !== undefined) {
          parts.push(el('p', { class: 'window ' + (b.InWindow ? 'in' : 'out') },
            b.InWindow ? 'In maintenance window' : 'Outside maintenance window',
            b.Window ? ' (' + b.Window + ')' : ''));
        }
        if (b.DryRun) parts.push(el('p', { class: 'ok' }, 'Dry run — nothing was changed.'));
        if (b.Applied) parts.push(el('p', { class: 'ok' }, 'Applied.'));
        if (b.FollowUpID > 0) {
          parts.push(el('p', { class: 'ok' }, 'Follow-up queued (#' + b.FollowUpID + ') — the next step continues in turn.'));
        }
        if (!parts.length) parts.push(el('p', { class: 'muted' }, 'Done — no details returned.'));
      }
      return el('div', {}, parts);
    }

    function applyError(e) {
      if (e.status === 422) {
        // {"error":"apply blocked","reasons":[...]} — reasons verbatim.
        return blockedBox('Apply blocked', e.body && e.body.reasons);
      }
      return el('p', { class: 'err' }, (e.body && e.body.error) || String(e.message || e));
    }

    function blockedBox(title, reasons) {
      var lis = (reasons || []).map(function (r) { return el('li', {}, r); });
      var box = el('div', { class: 'blocked' },
        el('p', { class: 'blocked-title' }, title),
        lis.length ? el('ul', { class: 'reasons' }, lis) : el('p', { class: 'muted' }, 'No reasons given.'));
      return box;
    }

    overlay.addEventListener('click', function (e) { if (e.target === overlay) close(); });

    // appendChild takes ONE node — append the panel's children one at a
    // time (a multi-arg call would silently drop every child but the first).
    [
      el('div', { class: 'modal-head' },
        el('h2', { class: 'modal-title' }, 'Apply recommendation #' + rec.ID),
        el('p', { class: 'modal-sub' }, rec.WorkloadName + ' · ' + rec.Namespace + ' · ' + rec.Resource)),
      el('div', { class: 'modal-change' }, recChange(rec)),
      el('div', { class: 'seg-row' }, segBtns),
      actorWrap,
      result,
      el('div', { class: 'modal-foot' }, cancelBtn, runBtn)
    ].forEach(function (c) { panel.appendChild(c); });
    overlay.appendChild(panel);
    document.body.appendChild(overlay);
  }

  // Force the next render of a view to refetch (stale <30s data skipped).
  function markStale(name) {
    var v = VIEWS[name];
    if (v) v.loadedAt = 0;
  }

  /* ---------- audit timeline ---------- */

  // One merged timeline: every apply event with its diff, actor, step,
  // and mode; the matching verification run (verdict + SLI evidence)
  // nests beneath the event it judged.
  function applyTimeline(applies, runsByEvent, nameOf) {
    if (!applies.length) return emptyState('No apply events yet — the trail begins with the first apply.');
    var ul = el('ul', { class: 'timeline' });
    applies.forEach(function (e) {
      var run = runsByEvent.get(e.ID);
      var dot = e.Result === 'applied' ? 'green' : e.Result === 'reverted' ? 'red' : 'gray';
      var card = el('div', { class: 'tl-card' },
        el('div', { class: 'tl-top' },
          el('span', { class: 'mono muted' }, '#' + e.ID),
          resultBadge(e.Result),
          el('span', { class: 'chip' }, e.Mode),
          el('span', { class: 'mono muted' }, 'step ' + e.StepNumber + '/' + e.TotalSteps),
          nameOf ? el('span', { class: 'muted' }, nameOf(e.WorkloadID)) : null,
          el('span', { class: 'tl-change' }, diffCell(e.Diff)),
          el('span', { class: 'tl-meta' }, (e.Actor || '—') + ' · ' + fmtTime(e.CreatedAt))));
      if (run) {
        card.appendChild(el('div', { class: 'tl-verdict' },
          el('div', { class: 'row' },
            verdictBadge(run.Verdict),
            el('span', { class: 'tl-evidence', title: sliSummary(run.SLIs) },
              'SLIs: ' + sliSummary(run.SLIs)),
            el('span', { class: 'tl-meta' }, 'run #' + run.ID + ' · ' + fmtTime(run.CreatedAt))),
          el('div', { class: 'row' },
            'baseline ', fmtTime(run.BaselineStart), ' → ', fmtTime(run.BaselineEnd),
            ' · post ', fmtTime(run.PostStart), ' → ', fmtTime(run.PostEnd))));
      } else if (e.Result === 'applied') {
        card.appendChild(el('div', { class: 'tl-verdict' },
          el('span', { class: 'tl-empty' }, 'no verification run yet')));
      }
      ul.appendChild(el('li', {}, el('span', { class: 'tl-dot ' + dot }), card));
    });
    return ul;
  }

  function renderAudit(root) {
    root.replaceChildren(loadingState('Loading audit trail…'));
    return Promise.all([api.applies(), api.verificationRuns(), api.workloads()]).then(function (results) {
      var applies = results[0] || [];
      var runs = results[1] || [];
      var workloads = results[2] || [];
      var names = new Map(workloads.map(function (w) { return [w.ID, w.Namespace + '/' + w.Name]; }));
      var nameOf = function (id) { return names.get(id) || 'workload #' + id; };
      var runsByEvent = new Map();
      runs.forEach(function (r) { runsByEvent.set(r.ApplyEventID, r); });
      root.replaceChildren(
        pageHead('Audit', 'Every apply event, its diff, and its verification verdict — INSERT-only, never rewritten.'),
        el('div', { class: 'card' },
          el('div', { class: 'card-head' },
            el('h2', { class: 'card-title' }, 'Apply & verification timeline'),
            el('span', { class: 'card-sub' }, applies.length + ' events · ' + runs.length + ' verification runs')),
          applyTimeline(applies, runsByEvent, nameOf)));
    }, function (err) {
      root.replaceChildren(errorState('Audit trail failed to load.'));
    });
  }

  /* ---------- usage chart (pure canvas, no libraries) ---------- */

  var SERIES = [
    { key: 'p50', label: 'p50', color: 'var(--p50)' },
    { key: 'p95', label: 'p95', color: 'var(--p95)' },
    { key: 'p99', label: 'p99', color: 'var(--p99)' },
    { key: 'max', label: 'max', color: 'var(--pmax)' }
  ];

  // Chart metric toggles per surface. DB workloads store db_* percent /
  // iops / connections metrics; compute workloads store raw k8s
  // millicores / bytes (the series endpoint's `unit` matches these
  // labels — see the API contract).
  var DB_METRICS = [
    { key: 'cpu_percent', label: 'CPU %', pct: true, yMax: 100 },
    { key: 'mem_percent', label: 'Memory %', pct: true, yMax: 100 },
    { key: 'iops', label: 'IOPS', pct: false, yMax: 0 },
    { key: 'connections', label: 'Connections', pct: false, yMax: 0 }
  ];
  var COMPUTE_METRICS = [
    { key: 'cpu_percent', label: 'CPU (millicores)', pct: false, yMax: 0 },
    { key: 'mem_percent', label: 'Memory (bytes)', pct: false, yMax: 0 }
  ];

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  function niceMax(v) {
    if (!(v > 0)) return 100;
    var mag = Math.pow(10, Math.floor(Math.log10(v)));
    var n = v / mag;
    var step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10;
    return step * mag;
  }

  function fmtChartValue(v, pctMetric) {
    return pctMetric ? Math.round(v) + '%' : Math.round(v).toLocaleString('en-US');
  }

  function fmtDate(ts) {
    return new Date(ts).toLocaleString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  }

  function fmtAxisDate(ts) {
    return new Date(ts).toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
  }

  // Normalize several plausible series response shapes (the endpoint
  // contract is landing in parallel): envelope keys, bare arrays,
  // timestamp names, percentile names.
  function normalizeSeries(body) {
    if (!body || typeof body !== 'object') return [];
    var arr = Array.isArray(body)
      ? body
      : pick(body, ['series', 'points', 'buckets', 'data', 'rows', 'values']);
    if (!Array.isArray(arr)) return [];
    var out = [];
    arr.forEach(function (pt) {
      if (!pt || typeof pt !== 'object') return;
      var ts = pick(pt, ['ts', 't', 'time', 'timestamp', 'window_start', 'windowStart', 'date']);
      var ms = typeof ts === 'number' ? ts : Date.parse(ts);
      if (isNaN(ms)) return;
      out.push({
        ts: ms,
        p50: pick(pt, ['p50', 'p50th', 'percentile_50', 'P50']),
        p95: pick(pt, ['p95', 'p95th', 'percentile_95', 'P95']),
        p99: pick(pt, ['p99', 'p99th', 'percentile_99', 'P99']),
        max: pick(pt, ['max', 'maximum', 'Max'])
      });
    });
    out.sort(function (a, b) { return a.ts - b.ts; });
    return out;
  }

  function chartEmpty() {
    return el('div', { class: 'chart-empty' },
      el('strong', {}, 'No chart data yet'),
      el('span', {}, 'The series endpoint has no data for this workload and metric yet.'));
  }

  function chartCard(workloadID, isDB) {
    var metrics = isDB ? DB_METRICS : COMPUTE_METRICS;
    var metric = metrics[0];
    var points = [];
    var hover = -1;
    var busy = false;

    var holder = el('div', { class: 'chart-holder' });
    var canvas = el('canvas', { class: 'chart' });
    var tip = el('div', { class: 'chart-tip' });

    function geom() {
      var w = Math.max(200, holder.clientWidth || 600);
      var h = 280;
      var padL = 46, padR = 18, padT = 12, padB = 26;
      var plotW = Math.max(1, w - padL - padR);
      var plotH = Math.max(1, h - padT - padB);
      var t0 = points[0].ts, t1 = points[points.length - 1].ts;
      var span = t1 - t0 || 1;
      var yMax = metric.yMax || niceMax(maxValue());
      return {
        w: w, h: h, padL: padL, padR: padR, plotW: plotW, plotH: plotH, t0: t0, span: span, yMax: yMax,
        x: function (t) { return padL + ((t - t0) / span) * plotW; },
        y: function (v) { return padT + plotH - (v / yMax) * plotH; }
      };
    }

    function maxValue() {
      var m = 0;
      points.forEach(function (p) {
        SERIES.forEach(function (s) {
          var v = p[s.key];
          if (typeof v === 'number' && v > m) m = v;
        });
      });
      return m;
    }

    function draw(ctx, g, alpha, withHover) {
      ctx.clearRect(0, 0, g.w, g.h);
      if (alpha != null) ctx.globalAlpha = alpha;

      // Gridlines + y labels.
      ctx.font = '10.5px ' + cssVar('--sans');
      ctx.textBaseline = 'middle';
      for (var i = 0; i <= 4; i++) {
        var v = g.yMax * i / 4;
        var gy = Math.round(g.y(v)) + .5;
        ctx.strokeStyle = 'rgba(15, 23, 42, .06)';
        ctx.lineWidth = 1;
        ctx.beginPath(); ctx.moveTo(g.padL, gy); ctx.lineTo(g.w - g.padR, gy); ctx.stroke();
        ctx.fillStyle = cssVar('--muted');
        ctx.textAlign = 'right';
        ctx.fillText(fmtChartValue(v, metric.pct), g.padL - 7, gy);
      }

      // X ticks.
      ctx.textAlign = 'center';
      ctx.textBaseline = 'top';
      for (var t = 0; t <= 4; t++) {
        var ts = g.t0 + g.span * t / 4;
        ctx.fillText(fmtAxisDate(ts), g.x(ts), g.h - 18);
      }

      // Percentile lines, lightest first so max rides on top.
      SERIES.forEach(function (s) {
        ctx.strokeStyle = s.color.indexOf('var(') === 0 ? cssVar(s.color.slice(4, -1)) : s.color;
        ctx.lineWidth = 2;
        ctx.lineJoin = 'round';
        ctx.lineCap = 'round';
        ctx.beginPath();
        var started = false;
        points.forEach(function (p) {
          var v = p[s.key];
          if (typeof v !== 'number') return;
          var px = g.x(p.ts), py = g.y(v);
          if (!started) { ctx.moveTo(px, py); started = true; }
          else ctx.lineTo(px, py);
        });
        ctx.stroke();
      });

      // Single-point case: markers instead of degenerate lines.
      if (points.length === 1) {
        SERIES.forEach(function (s) {
          var v = points[0][s.key];
          if (typeof v !== 'number') return;
          ctx.fillStyle = s.color.indexOf('var(') === 0 ? cssVar(s.color.slice(4, -1)) : s.color;
          ctx.beginPath();
          ctx.arc(g.x(points[0].ts), g.y(v), 4, 0, Math.PI * 2);
          ctx.fill();
        });
      }

      // End labels — only when every line end clears its neighbor by
      // 13px, otherwise the legend + tooltip carry identity.
      if (alpha == null && points.length > 1) {
        var ys = SERIES.map(function (s) {
          var p = points[points.length - 1];
          return { s: s, y: g.y(p[s.key]) };
        });
        var spread = true;
        for (var j = 1; j < ys.length; j++) {
          if (Math.abs(ys[j].y - ys[j - 1].y) < 13) { spread = false; break; }
        }
        if (spread) {
          ctx.textBaseline = 'middle';
          ys.forEach(function (y1) {
            var col = y1.s.color.indexOf('var(') === 0 ? cssVar(y1.s.color.slice(4, -1)) : y1.s.color;
            var xEnd = g.x(g.t0 + g.span);
            ctx.strokeStyle = col;
            ctx.lineWidth = 2;
            ctx.beginPath(); ctx.moveTo(xEnd, y1.y); ctx.lineTo(xEnd + 7, y1.y); ctx.stroke();
            ctx.fillStyle = cssVar('--ink-2');
            ctx.textAlign = 'left';
            var label = y1.s.label + ' ' + fmtChartValue(points[points.length - 1][y1.s.key], metric.pct);
            if (xEnd + 14 + ctx.measureText(label).width <= g.w - 4) {
              ctx.fillText(label, xEnd + 12, y1.y);
            }
          });
        }
      }

      if (alpha != null) ctx.globalAlpha = 1;
      if (withHover && hover >= 0) {
        var hx = Math.round(g.x(points[hover].ts)) + .5;
        ctx.strokeStyle = 'rgba(15, 23, 42, .22)';
        ctx.lineWidth = 1;
        ctx.beginPath(); ctx.moveTo(hx, g.padT); ctx.lineTo(hx, g.h - 26); ctx.stroke();
      }
    }

    function paint(alpha, withHover) {
      if (!canvas.isConnected) return;
      var dpr = window.devicePixelRatio || 1;
      var g = geom();
      canvas.width = Math.round(g.w * dpr);
      canvas.height = Math.round(g.h * dpr);
      var ctx = canvas.getContext('2d');
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      draw(ctx, g, alpha, withHover);
    }

    function updateTip(idx) {
      var g = geom();
      var pt = points[idx];
      if (!pt) return;
      var rows = SERIES.map(function (s) {
        var v = pt[s.key];
        return el('div', { class: 'tip-row' },
          el('span', { class: 'tip-swatch', style: 'border-color:' + (s.color.indexOf('var(') === 0 ? cssVar(s.color.slice(4, -1)) : s.color) }),
          s.label,
          el('span', { class: 'tip-val' }, typeof v === 'number' ? fmtChartValue(v, metric.pct) : '—'));
      });
      // replaceChildren does not flatten arrays — spread the rows in.
      tip.replaceChildren(el('div', { class: 'tip-date' }, fmtDate(pt.ts)), ...rows);
      tip.classList.add('show');
      var x = g.x(pt.ts);
      var left = x + 16, top = g.y(0) - tip.offsetHeight - 10;
      if (left + tip.offsetWidth > holder.clientWidth - 8) left = x - tip.offsetWidth - 16;
      if (top < 4) top = 4;
      tip.style.left = Math.max(4, left) + 'px';
      tip.style.top = top + 'px';
    }

    canvas.addEventListener('pointermove', function (e) {
      if (!points.length || busy) return;
      var rect = canvas.getBoundingClientRect();
      var g = geom();
      var px = e.clientX - rect.left;
      var idx = Math.round(((px - g.padL) / g.plotW) * (points.length - 1));
      idx = Math.max(0, Math.min(points.length - 1, idx));
      if (idx !== hover) { hover = idx; paint(null, true); }
      updateTip(idx);
    });
    canvas.addEventListener('pointerleave', function () {
      hover = -1;
      tip.classList.remove('show');
      paint(null, false);
    });

    var segs = metrics.map(function (m) {
      var b = el('button', { class: 'seg' + (m === metric ? ' active' : '') }, m.label);
      b.addEventListener('click', function () {
        if (m === metric || busy) return;
        metric = m;
        segs.forEach(function (x) { x.classList.toggle('active', x === b); });
        refetch(true);
      });
      return b;
    });

    var legend = el('div', { class: 'chart-legend' },
      SERIES.map(function (s) {
        return el('span', { class: 'key' },
          el('span', { class: 'sw ' + (s.key === 'max' ? 'pmax' : s.key) }),
          s.label);
      }));

    var chip = el('div', { class: 'chart-loading', hidden: true }, 'loading…');

    function refetch(keepFrame) {
      busy = true;
      if (points.length && keepFrame) {
        chip.hidden = false;
        paint(.35, false);
      } else if (!points.length) {
        holder.replaceChildren(el('div', { class: 'chart-empty' },
          el('strong', {}, 'Loading usage data…')));
      }
      api.series(workloadID, { metric: metric.key, days: 14 }).then(function (body) {
        points = normalizeSeries(body);
        busy = false;
        chip.remove();
        hover = -1;
        if (!points.length) { holder.replaceChildren(chartEmpty()); return; }
        holder.replaceChildren(canvas, tip, chip);
        paint(null, false);
      }, function () {
        points = [];
        busy = false;
        chip.remove();
        hover = -1;
        holder.replaceChildren(chartEmpty());
      });
    }

    var card = el('div', { class: 'card chart-card' },
      el('div', { class: 'card-head' },
        el('h2', { class: 'card-title' }, 'Usage — last 14 days'),
        el('span', { class: 'card-sub' }, 'observed percentiles, 15-minute buckets')),
      el('div', { class: 'chart-toolbar' },
        legend,
        el('div', { class: 'filter-row' }, el('span', { class: 'seg-row' }, segs))),
      holder);

    window.addEventListener('resize', function () {
      if (points.length && canvas.isConnected) paint(null, false);
    });

    return { card: card, start: function () { refetch(false); } };
  }

  /* ---------- workload detail ---------- */

  function kvRows(rows) {
    return el('div', { class: 'kv' }, rows.map(function (r) {
      return el('div', { class: 'row' },
        el('span', { class: 'k' }, r[0]),
        el('span', { class: 'v' + (r[2] ? ' mono' : '') }, r[1]));
    }));
  }

  function recommendationsForDetail(recs) {
    if (!recs.length) {
      return el('div', { class: 'state' },
        el('p', { class: 'state-title' }, 'No recommendations yet'),
        el('p', { class: 'state-sub' }, 'Run the analyzer for this workload to see proposed changes.'));
    }
    return el('div', { class: 'tbl-wrap' },
      el('table', {},
        theadRow([
          { label: 'Resource' }, { label: 'One-step plan' },
          { label: 'Savings / mo', right: true }, { label: 'Confidence', right: true },
          { label: 'Status' }, { label: 'Created' }, { label: 'Apply' }
        ]),
        el('tbody', {}, recs.map(function (r) {
          return el('tr', {},
            td(undefined, el('span', { class: 'chip ' + r.Resource }, r.Resource)),
            td(undefined, recChange(r)),
            td('num mono', money(r.SavingsMonthly)),
            td('num mono', pct(r.Confidence)),
            td(undefined, statusBadge(r.Status)),
            td('muted', fmtTime(r.CreatedAt)),
            td(undefined, r.Status === 'pending'
              ? el('button', { class: 'btn small', onclick: function () { openApplyModal(r); } }, 'Apply')
              : el('span', { class: 'muted' }, '—')));
        }))));
  }

  function applyHistory(applies, runsByEvent) {
    return applyTimeline(applies, runsByEvent, null);
  }

  function renderWorkloadDetail(root) {
    var id = currentDetailId();
    if (!id) { root.replaceChildren(errorState('Unknown workload.')); return; }
    root.replaceChildren(loadingState('Loading workload…'));
    return Promise.all([
      api.workload(id),
      api.recommendations({ workload_id: id, limit: 100 }),
      api.applies({ workload_id: id }),
      api.verificationRuns()
    ]).then(function (results) {
      var wl = results[0];
      var recs = (results[1] && results[1].recommendations) || [];
      var applies = results[2] || [];
      var runs = results[3] || [];
      var runsByEvent = new Map();
      runs.forEach(function (r) { runsByEvent.set(r.ApplyEventID, r); });

      var isDB = wl.Source === 'db' || wl.Kind === 'database';

      var curRows;
      if (isDB) {
        curRows = [
          ['Instance class', el('span', { class: 'chip class' }, wl.DBClass || '—')],
          ['Replicas', String(wl.DBReplicas), true],
          ['Maintenance window', wl.DBMaintenanceWindow || '—', true],
          ['Provider', wl.DBProvider || '—']
        ];
      } else {
        curRows = [
          ['CPU request', fmtMilli(wl.RequestCPUMilli), true],
          ['CPU limit', fmtMilli(wl.LimitCPUMilli), true],
          ['Memory request', fmtBytes(wl.RequestMemBytes), true],
          ['Memory limit', fmtBytes(wl.LimitMemBytes), true]
        ];
      }

      var chart = chartCard(wl.ID, isDB);

      var header = el('div', { class: 'wl-head' },
        avatarEl(wl.Name, isDB ? 'db' : 'k8s', true),
        el('div', {},
          el('h1', {}, wl.Name),
          el('span', { class: 'ns' }, wl.Namespace)),
        el('div', { class: 'badges' },
          kindBadge(isDB),
          riskOf(wl) != null ? riskBadge(riskOf(wl), riskReasonsOf(wl)) : null,
          isDB ? el('span', { class: 'chip prov' }, wl.DBProvider || 'db') : el('span', { class: 'chip k8s' }, 'k8s')));

      root.replaceChildren(
        el('a', { class: 'back-link', href: '#/workloads' }, '← All workloads'),
        header,
        el('div', { class: 'meta-grid' },
          card('Current state', kvRows(curRows)),
          card('Recommendations', recommendationsForDetail(recs))),
        chart.card,
        card('Apply history',
          applyHistory(applies, runsByEvent),
          applies.length ? applies.length + ' events for this workload' : null));
      chart.start();
    }, function (err) {
      if (String(err && err.message || err).indexOf('404') !== -1) {
        root.replaceChildren(notFoundState('Workload not found',
          'It may have been removed from the managed surface.', '#/workloads', 'Back to workloads'));
        return;
      }
      root.replaceChildren(errorState('Workload failed to load.'));
    });
  }

  /* ---------- routing ---------- */

  var TABS = ['dashboard', 'recommendations', 'workloads', 'audit'];

  var VIEWS = {
    dashboard: { run: function () { return renderDashboard(document.getElementById('view-dashboard')); } },
    recommendations: { run: function () { return renderRecommendations(document.getElementById('view-recommendations')); } },
    workloads: { run: function () { return renderWorkloads(document.getElementById('view-workloads')); } },
    audit: { run: function () { return renderAudit(document.getElementById('view-audit')); } },
    'workload-detail': { run: function () { return renderWorkloadDetail(document.getElementById('view-workload-detail')); } }
  };

  var VIEW_SECTIONS = ['dashboard', 'recommendations', 'workloads', 'audit', 'workload-detail'];

  function currentDetailId() {
    var parts = location.hash.replace(/^#\/?/, '').toLowerCase().split('/').filter(Boolean);
    return parts.length === 2 && parts[0] === 'workloads' && /^\d+$/.test(parts[1]) ? parts[1] : null;
  }

  function parseRoute() {
    var parts = location.hash.replace(/^#\/?/, '').toLowerCase().split('/').filter(Boolean);
    var id = currentDetailId();
    if (id) return { tab: 'workloads', view: 'workload-detail' };
    var name = TABS.indexOf(parts[0]) !== -1 ? parts[0] : 'dashboard';
    return { tab: name, view: name };
  }

  function route() {
    var r = parseRoute();

    TABS.forEach(function (name) {
      var on = name === r.tab;
      var tab = document.querySelector('.tab[data-view="' + name + '"]');
      tab.classList.toggle('active', on);
      if (on) tab.setAttribute('aria-current', 'page');
      else tab.removeAttribute('aria-current');
    });
    VIEW_SECTIONS.forEach(function (name) {
      document.getElementById('view-' + name).hidden = name !== r.view;
    });

    updateApiStatus();

    var view = VIEWS[r.view];
    // Changing workload id must force a refetch even inside 30s.
    if (r.view === 'workload-detail' && view.lastId !== currentDetailId()) {
      view.lastId = currentDetailId();
      view.loadedAt = 0;
    }
    // Views refetch when >30s stale; already-pending loads are awaited.
    var fresh = view.loaded && Date.now() - (view.loadedAt || 0) < 30000;
    if (fresh) return;
    view.loadedAt = Date.now();
    view.loaded = true;
    view.run();
  }

  window.addEventListener('hashchange', route);
  route();
})();
