/* api.js — the only file that talks to the backend.
 *
 * One function per endpoint, every one taking an optional `params`
 * object whose entries become query-string parameters, passed through
 * verbatim (e.g. pagination: { limit, offset }, filters: { status,
 * workload_id, result }, series: { metric, days }).
 *
 * Base URL: the API is served at /api/v1 on the same origin. For dev
 * against a remote API while the page is served from elsewhere, set
 * `window.CONSIZE_API_BASE` (e.g. "http://localhost:8080/api/v1")
 * before this script loads.
 *
 * Field names on the returned objects are the store's Go struct fields
 * verbatim (PascalCase) — see engine/internal/store/store.go. Response
 * envelopes are unwrapped to arrays here, except recommendations(),
 * which returns the full body {recommendations, pagination} because the
 * load-more flow needs pagination.total, and savings()/series(), whose
 * shapes the app feature-checks defensively (a parallel backend effort
 * is extending both).
 */
(function () {
  'use strict';

  var base = function () {
    return (window.CONSIZE_API_BASE || '/api/v1').replace(/\/+$/, '');
  };

  function queryString(params) {
    if (!params) return '';
    var pairs = Object.keys(params)
      .filter(function (k) { return params[k] !== undefined && params[k] !== null && params[k] !== ''; })
      .map(function (k) { return encodeURIComponent(k) + '=' + encodeURIComponent(params[k]); });
    return pairs.length ? '?' + pairs.join('&') : '';
  }

  // Origin of the API server — /healthz and /readyz live on the router
  // ROOT, not under /api/v1 (engine/internal/api/server.go).
  function origin() {
    return new URL(base(), window.location.origin).origin;
  }

  function fetchPath(path, params) {
    return fetch(path + queryString(params)).then(function (res) {
      if (!res.ok) {
        return res.json().then(function (body) {
          throw new Error('HTTP ' + res.status + ' on ' + path + (body && body.error ? ': ' + body.error : ''));
        });
      }
      return res.json();
    });
  }

  function request(apiPath, params) { return fetchPath(base() + apiPath, params); }

  // POST with a JSON body. On non-2xx the parsed body travels on the
  // error: err.status + err.body — the apply endpoint answers 422 with
  // {"error":"apply blocked","reasons":[...]} and the UI renders the
  // reasons verbatim, so callers must see them.
  function post(apiPath, payload) {
    return fetch(base() + apiPath, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload || {})
    }).then(function (res) {
      return res.json().then(function (json) {
        if (!res.ok) {
          var e = new Error('HTTP ' + res.status + ' on ' + apiPath);
          e.status = res.status;
          e.body = json;
          throw e;
        }
        return json;
      });
    });
  }

  window.ConsizeAPI = {
    base: base,
    /* GET /healthz, /readyz — mounted at the router root, not /api/v1 */
    healthz: function () { return fetchPath(origin() + '/healthz'); },
    readyz: function () { return fetchPath(origin() + '/readyz'); },

    /* GET /workloads -> {workloads: [...]} */
    workloads: function (params) { return request('/workloads', params).then(function (b) { return b.workloads; }); },
    /* GET /workloads/{id} -> single workload object */
    workload: function (id) { return request('/workloads/' + id); },

    /* GET /recommendations?status=&workload_id=&limit=&offset=
     * -> {recommendations: [...], pagination: {limit, offset, total}} */
    recommendations: function (params) { return request('/recommendations', params); },

    /* GET /savings -> {projected_monthly_savings, active_recommendations,
     * price_table} — a parallel backend effort extends it with realized
     * savings and a per-owner breakdown; this file passes the body
     * through and app.js feature-checks whichever fields exist. */
    savings: function () { return request('/savings'); },

    /* GET /workloads/{id}/series?metric=&days=
     * -> backend contract in flight; app.js normalizes several candidate
     * shapes and degrades to "No chart data yet" on 404/empty. */
    series: function (id, params) { return request('/workloads/' + id + '/series', params); },

    /* GET /applies?workload_id=&result= -> {applies: [...]} */
    applies: function (params) { return request('/applies', params).then(function (b) { return b.applies; }); },

    /* GET /verification-runs?apply_event_id= -> {verification_runs: [...]} */
    verificationRuns: function (params) {
      return request('/verification-runs', params).then(function (b) { return b.verification_runs; });
    },

    /* POST /recommendations/{id}/apply — body {"mode","actor"}. The route
     * picks the engine from the recommendation's resource (class -> DB
     * engine, cpu/memory -> k8s engine). Resolves with the apply result
     * {EventID, DryRun, Applied, Diff, StepNumber, TotalSteps,
     * FollowUpID, Blocked, BlockReasons[, InWindow, Window]}; rejects
     * with err.status/err.body for 4xx/5xx. */
    apply: function (id, payload) { return post('/recommendations/' + id + '/apply', payload); }
  };
})();
