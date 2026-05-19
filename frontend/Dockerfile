# syntax=docker/dockerfile:1.7
# Dev-mode image for the Svelte frontend. Production (PR #4-5) will switch
# to a multi-stage build whose `vite build` output is copied into the Go
# binary via embed.FS — there's no need to ship Node in production.

FROM node:24-alpine
WORKDIR /app

# Install deps first so editing source doesn't bust the layer cache.
# package-lock.json may not exist on the first run; that's fine, npm
# will create it inside the container.
COPY package.json package-lock.json* ./
RUN npm install

COPY . .

EXPOSE 5173
# --host 0.0.0.0 so the dev server is reachable from the host.
# HMR over WebSocket works through the published port; CHOKIDAR_USEPOLLING
# (set in compose) keeps file-change detection reliable on Docker Desktop
# Mac/Windows where inotify across the bind-mount boundary is flaky.
CMD ["npm", "run", "dev", "--", "--host", "0.0.0.0"]
