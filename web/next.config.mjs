/** @type {import('next').NextConfig} */

// The UI talks to the Go API (`drive-cleanup review`) through same-origin
// /api/* paths, proxied here so there is no CORS setup. Override the target
// with REVIEW_API if the Go server runs elsewhere.
const REVIEW_API = process.env.REVIEW_API ?? 'http://127.0.0.1:8844';

const nextConfig = {
  async rewrites() {
    return [{ source: '/api/:path*', destination: `${REVIEW_API}/api/:path*` }];
  },
};

export default nextConfig;
