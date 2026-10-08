import react from '@vitejs/plugin-react';
import { defineConfig, loadEnv } from 'vite';

export default defineConfig(({ mode }) => {
  const envDir = process.env.GLOWBOM_RELEASE_BUILD === '1' ? '/nonexistent-glowbom-release-env' : process.cwd();
  const env = loadEnv(mode, envDir, '');
  const backendTarget = env.VITE_BACKEND_TARGET || 'http://127.0.0.1:4569';
  const port = Number(env.GLOWBOM_WEB_PORT || '4572');
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('Invalid GLOWBOM_WEB_PORT');

  return {
    envDir,
    base: env.VITE_APP_BASE_PATH || (mode === 'production' ? './' : '/'),
    plugins: [react(), {
      name: 'glowbom-launch-identity',
      configureServer(server) {
        server.middlewares.use('/__glowbom/launch', (request, response) => {
          if (request.method !== 'GET') { response.statusCode = 405; response.end(); return; }
          response.setHeader('Content-Type', 'application/json');
          response.setHeader('Cache-Control', 'no-store');
          response.end(JSON.stringify({ instance: env.GLOWBOM_INSTANCE || 'oss', launchId: env.GLOWBOM_LAUNCH_ID || '' }));
        });
      },
    }],
    server: {
      cors: false,
      host: '127.0.0.1',
      port,
      strictPort: true,
      proxy: { '/api': { target: backendTarget, changeOrigin: true, rewrite: path => path.replace(/^\/api/, '') } },
    },
  };
});
