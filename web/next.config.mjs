/** @type {import('next').NextConfig} */

// The UI talks to the Go API (`drive-cleanup review`) through same-origin
// /api/* paths, proxied here so there is no CORS setup. Override the target
// with REVIEW_API if the Go server runs elsewhere.
const REVIEW_API = process.env.REVIEW_API ?? 'http://127.0.0.1:8844';

const nextConfig = {
  async rewrites() {
    return [{ source: '/api/:path*', destination: `${REVIEW_API}/api/:path*` }];
  },

  // `next dev` only serves its dev-only assets (/_next/*, HMR) to origins it
  // was started on. When the UI is reached through an ngrok tunnel the browser
  // sends the tunnel host instead of localhost, so allow ngrok's domains too —
  // see scripts/share-review.sh. This is a development-only setting; access is
  // gated at the ngrok edge by ngrok/traffic-policy.yml.
  allowedDevOrigins: ['*.ngrok.app', '*.ngrok-free.app', '*.ngrok.dev', '*.ngrok.io'],
};

export default nextConfig;
