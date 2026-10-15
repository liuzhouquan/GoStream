import http from 'k6/http';
import { check, sleep } from 'k6';

const base = (__ENV.BASE_URL || 'http://127.0.0.1:8080').replace(/\/$/, '');
const scene = __ENV.SCENE || 'timeline';

export const options = {
  vus: Number(__ENV.VUS || 10),
  duration: __ENV.DURATION || '30s',
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<500'],
  },
};

export default function () {
  const res = http.get(`${base}/api/feed?scene=${encodeURIComponent(scene)}&limit=10`);
  check(res, {
    'feed responds 200': (r) => r.status === 200,
    'response contains items': (r) => Array.isArray(r.json('items')),
  });
  sleep(0.2);
}
