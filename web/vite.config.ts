import react from '@vitejs/plugin-react';
import { defineConfig, loadEnv } from 'vite';

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '');
  const backendTarget = env.VITE_BACKEND_TARGET || 'http://127.0.0.1:4569';
  const appBasePath = env.VITE_APP_BASE_PATH || (mode === 'production' ? './' : '/');
  const host = env.GLOWBOM_BIND_HOST || env.GLOWBY_BIND_HOST || '127.0.0.1';

  return {
    base: appBasePath,
    plugins: [react()],
    server: {
      cors: false,
      port: 4572,
      strictPort: true,
      host,
      proxy: {
        '/api': {
          target: backendTarget,
          changeOrigin: true,
          rewrite: (path) => path.replace(/^\/api/, ''),
        },
      },
    },
  };
});
