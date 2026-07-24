import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

export default defineConfig({
  root: "web",
  base: "/app/",
  plugins: [vue()],
  build: {
    outDir: "../public/app",
    emptyOutDir: true,
    rollupOptions: {
      output: {
        entryFileNames: "assets/app-[hash].js",
        chunkFileNames: "assets/[name].js",
        assetFileNames: (asset) => asset.name?.endsWith(".css") ? "assets/app-[hash][extname]" : "assets/[name]-[hash][extname]",
      },
    },
  },
});
