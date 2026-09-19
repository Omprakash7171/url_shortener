// Stage 21 load test.
// Scenarios-based so rates are arrival rates (rps), not VUs.
//
// Run:
//   docker run --rm -i grafana/k6 run - < scripts/k6-load.js
//
// Env overrides:
//   BASE         = base URL
//   RATE         = redirect requests/second
//   CODE         = short URL code
//   CREATE_RATE  = create requests/second

import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE || 'http://localhost:8090';
const CODE = __ENV.CODE || '1';

export const options = {
  scenarios: {
    // Cache-hot redirects: the primary production read path.
    redirects: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 400),
      timeUnit: '1s',
      duration: '60s',
      preAllocatedVUs: 100,
      maxVUs: 400,
      exec: 'redirects',
    },

    // Creates: intentionally low rate.
    // The API rate limiter may return 429 responses.
    creates: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.CREATE_RATE || 2),
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 10,
      maxVUs: 20,
      exec: 'creates',
    },
  },

  thresholds: {
    http_req_duration: ['p(99)<150'],
  },
};

// Redirect load test.
export function redirects() {
  const res = http.get(`${BASE}/${CODE}`, {
    discardResponseBodies: true,
    redirects: 0,
  });

  check(res, {
    'redirect 301': (r) => r.status === 301,
    'has Location': (r) => r.headers['Location'] !== undefined,
  });
}

// Create URL load test.
export function creates() {
  const body = JSON.stringify({
    url: `https://example.com/k6-${__VU}-${Date.now()}`,
  });

  const res = http.post(`${BASE}/api/v1/urls`, body, {
    headers: {
      'Content-Type': 'application/json',
    },
  });

  check(res, {
    'create 201': (r) => r.status === 201,

    // 429 is expected when the rate limiter is triggered.
    'create 201 or 429': (r) => r.status === 201 || r.status === 429,
  });
}
